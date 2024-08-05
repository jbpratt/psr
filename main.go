package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"cloud.google.com/go/storage"
	"github.com/GoogleCloudPlatform/testgrid/metadata"
	"github.com/GoogleCloudPlatform/testgrid/metadata/junit"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

type job struct {
	name     string
	id       string
	version  string
	duration string
	passed   bool
	suites   *junit.Suites
}

var (
	jobVersions = []string{"4.13"}             /*, "4.13", "4.14", "4.15", "4.16"} */
	jobNames    = []string{"rosa-classic-sts"} /*, "osd-aws", "osd-gcp"} */
	jobs        = make([]job, 0, len(jobNames))
)

const (
	jobPrefix           = "periodic-ci-openshift-osde2e-main-nightly-%s-"
	bucketName          = "test-platform-results"
	gcsBrowserURLPrefix = "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/test-platform-results/logs"
)

func processJobResults(suites *junit.Suites) {
	var passed, failed, skipped, pending int

	for _, suite := range suites.Suites {
		for _, result := range suite.Results {
			switch result.Status {
			case "passed":
				passed++
			case "failed":
				failed++
			case "skipped":
				skipped++
			case "pending":
				pending++
			}
		}
		// fmt.Printf("suite: %s %d %d %d\n", suite.Name, suite.Tests, suite.Failures, suite.Errors)
	}
}

func processJobXMLs(ctx context.Context, bucket *storage.BucketHandle, jobName, jobID string) error {
	objectIter := bucket.
		Objects(ctx, &storage.Query{MatchGlob: "**/*.xml", Prefix: fmt.Sprintf("logs/%s/%s/", jobName, jobID)})

	for {
		object, err := objectIter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}

		objectReader, err := bucket.Object(object.Name).NewReader(ctx)
		if err != nil {
			return err
		}

		suites, err := junit.ParseStream(objectReader)
		if err != nil {
			return err
		}
		_ = objectReader.Close()

		processJobResults(suites)
	}

	return nil
}

func processJob(ctx context.Context, bucket *storage.BucketHandle, name string) error {
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

	if err = processJobXMLs(ctx, bucket, name, jobID); err != nil {
		return err
	}

	duration := time.Duration(*finished.Timestamp-started.Timestamp) * time.Second
	jobs = append(jobs, job{
		name:     name,
		id:       jobID,
		passed:   *finished.Passed,
		duration: duration.String(),
		// suites:   suites,
	})
	return nil
}

func main() {
	ctx := context.Background()
	eg, _ := errgroup.WithContext(ctx)

	client, err := storage.NewClient(ctx, option.WithoutAuthentication())
	if err != nil {
		panic(err)
	}
	bucket := client.Bucket(bucketName)

	for _, jobName := range jobNames {
		for _, jobVersion := range jobVersions {
			name := fmt.Sprintf(jobPrefix+jobName, jobVersion)
			eg.Go(func() error { return processJob(ctx, bucket, name) })
		}
	}

	if err := eg.Wait(); err != nil {
		panic(err)
	}

	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].name < jobs[j].name
	})

	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	fmt.Fprintln(w, "name\tid\tduration\tpassed")
	for _, job := range jobs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\n", job.name, job.id, job.duration, job.passed)
	}
	if err := w.Flush(); err != nil {
		panic(err)
	}
}
