package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"cloud.google.com/go/storage"

	"github.com/GoogleCloudPlatform/testgrid/metadata"
	"github.com/GoogleCloudPlatform/testgrid/metadata/junit"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

var (
	jobVersions = []string{"4.13", "4.14", "4.15", "4.16"}
	jobNames    = []string{"rosa-classic-sts", "osd-aws", "osd-gcp"}
)

const (
	jobPrefix           = "periodic-ci-openshift-osde2e-main-nightly-%s-"
	bucketName          = "test-platform-results"
	gcsBrowserURLPrefix = "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/test-platform-results/logs"
)

type suiteResult struct {
	Name          string
	Tests         int
	Passed        int
	Failed        int
	Skipped       int
	Pending       int
	ErrorMessages string
}

type job struct {
	name     string
	id       string
	version  string
	duration time.Duration
	passed   bool
	results  []*suiteResult
}

type jobMonitoring struct {
	Summary string `json:"summary"`
	Errors  string `json:"errors"`
}

func sendSlackNotification(webhook string, summary, errors string) error {
	if webhook == "" {
		fmt.Println("Slack Webhook is not set, skipping notification.")
		return nil
	}

	if len(errors) > 4000 {
		errors = errors[:4000]
	}
	message := jobMonitoring{
		Summary: "Report: \n" + summary,
		Errors:  "Errors: \n" + errors,
	}

	jsonDataMessage, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("error marshalling summary to JSON: %w", err)
	}

	resp, err := http.Post(webhook, "application/json; charset=utf-8", bytes.NewBuffer(jsonDataMessage))
	if err != nil {
		return fmt.Errorf("error making request: %w", err)
	}
	defer resp.Body.Close()

	fmt.Printf("Job Monitoring Response status: %s\n", resp.Status)

	return nil
}

func processJobResults(suites *junit.Suites) []*suiteResult {
	var suitesResults []*suiteResult
	for _, suite := range suites.Suites {
		suiteResult := new(suiteResult)
		suiteResult.Name = suite.Name
		suiteResult.Tests = suite.Tests
		for _, result := range suite.Results {
			switch result.Status {
			case "passed":
				suiteResult.Passed++
			case "failed":
				suiteResult.Failed++
				suiteResult.ErrorMessages += fmt.Sprintf("Test Failed: %s\n: %s\n", result.Name, result.Failure.Value)
			case "skipped":
				suiteResult.Skipped++
			case "pending":
				suiteResult.Pending++
			case "":
				if result.Failure == nil {
					suiteResult.Passed++
				} else {
					suiteResult.Failed++
					suiteResult.ErrorMessages += fmt.Sprintf("Test Failed: %s\nFailure: %s\n", result.Name, result.Failure.Value)
				}
			}
		}
		suitesResults = append(suitesResults, suiteResult)
	}
	return suitesResults
}

func processJobXMLs(ctx context.Context, bucket *storage.BucketHandle, jobName, jobID string) ([]*suiteResult, error) {
	objectIter := bucket.
		Objects(ctx, &storage.Query{MatchGlob: "**/*.xml", Prefix: fmt.Sprintf("logs/%s/%s/", jobName, jobID)})

	jobResults := make([]*suiteResult, 0)

	for {
		object, err := objectIter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}

		objectReader, err := bucket.Object(object.Name).NewReader(ctx)
		if err != nil {
			return nil, err
		}

		suites, err := junit.ParseStream(objectReader)
		if err != nil {
			return nil, err
		}
		_ = objectReader.Close()

		jobResults = append(jobResults, processJobResults(suites)...)
	}

	return jobResults, nil
}

func processJob(ctx context.Context, bucket *storage.BucketHandle, name string, jobsChan chan<- *job) error {
	basePath := fmt.Sprintf("logs/%s", name)

	objReader, err := bucket.Object(basePath + "/latest-build.txt").NewReader(ctx)
	if err != nil {
		return err
	}
	latestBuildBytes, err := io.ReadAll(objReader)
	_ = objReader.Close()
	if err != nil {
		return err
	}

	job := &job{
		name: name,
		id:   string(latestBuildBytes),
	}

	basePath += fmt.Sprintf("/%s", job.id)

	var started metadata.Started
	var finished metadata.Finished

	objReader, err = bucket.Object(basePath + "/started.json").NewReader(ctx)
	if err != nil {
		return fmt.Errorf("fetch started.json for %s/%s: %w", name, job.id, err)
	}
	if err = json.NewDecoder(objReader).Decode(&started); err != nil {
		return err
	}

	objReader, err = bucket.Object(basePath + "/finished.json").NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			fmt.Printf("WARNING: job %s/%s finished.json not found, assuming job in-progress and skipping..\n", name, job.id)
			jobsChan <- job
			return nil
		}
		return fmt.Errorf("fetch finished.json for %s/%s: %w", name, job.id, err)
	}
	if err = json.NewDecoder(objReader).Decode(&finished); err != nil {
		return err
	}

	job.duration = time.Duration(*finished.Timestamp-started.Timestamp) * time.Second

	job.results, err = processJobXMLs(ctx, bucket, name, job.id)
	if err != nil {
		return err
	}

	jobsChan <- job

	return nil
}

func extractSummaryAndErrorsFromJobResult(job *job) (string, string) {
	var summaryBuilder strings.Builder
	var errorBuilder strings.Builder
	summaryBuilder.WriteString(fmt.Sprintf("Job: %s\n", job.name))
	for _, suite := range job.results {
		summaryBuilder.WriteString(fmt.Sprintf("\tsuite: %s %d %d %d %d %d\n", suite.Name, suite.Tests, suite.Passed, suite.Failed, suite.Skipped, suite.Pending))
		errorBuilder.WriteString(suite.ErrorMessages)
	}
	return summaryBuilder.String(), errorBuilder.String()
}

func main() {
	ctx := context.Background()
	eg, _ := errgroup.WithContext(ctx)

	client, err := storage.NewClient(ctx, option.WithoutAuthentication())
	if err != nil {
		panic(err)
	}
	bucket := client.Bucket(bucketName)

	jobsSize := len(jobNames) * len(jobVersions)
	jobsChan := make(chan *job, jobsSize)
	jobs := make([]*job, 0, jobsSize)

	for _, jobName := range jobNames {
		for _, jobVersion := range jobVersions {
			name := fmt.Sprintf(jobPrefix+jobName, jobVersion)
			eg.Go(func() error { return processJob(ctx, bucket, name, jobsChan) })
		}
	}

	if err := eg.Wait(); err != nil {
		panic(err)
	}

	for range jobsSize {
		job := <-jobsChan
		jobs = append(jobs, job)
	}

	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].name < jobs[j].name
	})

	tw := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	fmt.Fprintln(tw, "name\tid\tduration\tpassed")

	for _, job := range jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\n", job.name, job.id, job.duration, job.passed)

		// send slack message
	}

	if err := tw.Flush(); err != nil {
		panic(err)
	}
}
