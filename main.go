package main

import (
	"bytes"
	"cloud.google.com/go/storage"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/GoogleCloudPlatform/testgrid/metadata"
	"github.com/GoogleCloudPlatform/testgrid/metadata/junit"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

var (
	jobVersions = []string{"4.13", "4.14", "4.15", "4.16"}
	jobNames    = []string{"rosa-classic-sts", "osd-aws", "osd-gcp"}
	//jobs        = make([]job, 0, len(jobNames))
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

type jobResult struct {
	JobName      string
	SuitesResult []*suiteResult
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
		return fmt.Errorf("Error marshalling summary to JSON: %v\n", err)
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
	var _suitesResult []*suiteResult
	for _, suite := range suites.Suites {
		_suiteResult := new(suiteResult)
		_suiteResult.Name = suite.Name
		_suiteResult.Tests = suite.Tests
		for _, result := range suite.Results {
			switch result.Status {
			case "passed":
				_suiteResult.Passed++
			case "failed":
				_suiteResult.Failed++
				_suiteResult.ErrorMessages += fmt.Sprintf("Test Failed: %s\n: %s\n", result.Name, result.Failure.Value)
			case "skipped":
				_suiteResult.Skipped++
			case "pending":
				_suiteResult.Pending++
			case "":
				if result.Failure == nil {
					_suiteResult.Passed++
				} else {
					_suiteResult.Failed++
					_suiteResult.ErrorMessages += fmt.Sprintf("Test Failed: %s\nFailure: %s\n", result.Name, result.Failure.Value)
				}
			}
		}
		_suitesResult = append(_suitesResult, _suiteResult)
	}
	return _suitesResult
}

func processJobXMLs(ctx context.Context, bucket *storage.BucketHandle, jobName, jobID string) (*jobResult, error) {
	objectIter := bucket.
		Objects(ctx, &storage.Query{MatchGlob: "**/*.xml", Prefix: fmt.Sprintf("logs/%s/%s/", jobName, jobID)})
	_jobResult := new(jobResult)
	_jobResult.JobName = jobName
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
		_jobResult.SuitesResult = append(_jobResult.SuitesResult, processJobResults(suites)...)
	}
	return _jobResult, nil
}

func processJob(ctx context.Context, bucket *storage.BucketHandle, name string, logCh chan<- *jobResult) error {
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
	jobID := string(latestBuildBytes)

	basePath += fmt.Sprintf("/%s", jobID)

	var started metadata.Started
	var finished metadata.Finished

	objReader, err = bucket.Object(basePath + "/started.json").NewReader(ctx)
	if err != nil {
		return fmt.Errorf("fetch started.json for %s/%s: %w", name, jobID, err)
	}
	if err = json.NewDecoder(objReader).Decode(&started); err != nil {
		return err
	}

	objReader, err = bucket.Object(basePath + "/finished.json").NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			fmt.Printf("WARNING: job %s/%s finished.json not found, assuming job in-progress and skipping..\n", name, jobID)
			return nil
		}
		return fmt.Errorf("fetch finished.json for %s/%s: %w", name, jobID, err)
	}
	if err = json.NewDecoder(objReader).Decode(&finished); err != nil {
		return err
	}
	var _jobResult *jobResult
	_jobResult, err = processJobXMLs(ctx, bucket, name, jobID)
	if err != nil {
		return err
	}
	if _jobResult != nil {
		logCh <- _jobResult
	}
	//duration := time.Duration(*finished.Timestamp-started.Timestamp) * time.Second
	//jobs = append(jobs, job{
	//	name:     name,
	//	id:       jobID,
	//	passed:   *finished.Passed,
	//	duration: duration.String(),
	//	// suites:   suites,
	//})
	return nil
}

func extractSummaryAndErrorsFromJobResult(result *jobResult) (string, string) {
	var summaryBuilder strings.Builder
	var errorBuilder strings.Builder
	summaryBuilder.WriteString(fmt.Sprintf("Job: %s\n", result.JobName))
	for _, suite := range result.SuitesResult {
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
	jobResultChannel := make(chan *jobResult, jobsSize)
	var wg sync.WaitGroup

	// Goroutine to collect logs
	wg.Add(1)
	go func() {
		defer wg.Done()
		var statsSummaryBuilder strings.Builder
		var errorBuilder strings.Builder
		for _jobResult := range jobResultChannel {
			statsSummary, errorMessages := extractSummaryAndErrorsFromJobResult(_jobResult)
			statsSummaryBuilder.WriteString(statsSummary)
			errorBuilder.WriteString(errorMessages)
		}
		webhook := os.Getenv("WEBHOOK")
		err = sendSlackNotification(webhook, statsSummaryBuilder.String(), errorBuilder.String())
		if err != nil {
			panic(err)
		}
	}()

	for _, jobName := range jobNames {
		for _, jobVersion := range jobVersions {
			name := fmt.Sprintf(jobPrefix+jobName, jobVersion)
			eg.Go(func() error { return processJob(ctx, bucket, name, jobResultChannel) })
		}
	}

	if err := eg.Wait(); err != nil {
		panic(err)
	}
	close(jobResultChannel)
	wg.Wait()

	//sort.Slice(jobs, func(i, j int) bool {
	//	return jobs[i].name < jobs[j].name
	//})
	//
	//fmt.Println()
	//
	//w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	//fmt.Fprintln(w, "name\tid\tduration\tpassed")
	//for _, job := range jobs {
	//	fmt.Fprintf(w, "%s\t%s\t%s\t%t\n", job.name, job.id, job.duration, job.passed)
	//}
	//if err := w.Flush(); err != nil {
	//	panic(err)
	//}
}
