// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prowjob

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFinishedJSONURLFromViewURL(t *testing.T) {
	tests := []struct {
		name    string
		viewURL string
		want    string
		wantErr bool
	}{
		{
			name:    "typical prow deck view URL",
			viewURL: "https://prow.ci.openshift.org/view/gs/origin-ci-test/logs/branch-ci-Azure-ARO-HCP-main-e2e-stage-e2e-parallel/12345",
			want:    "https://storage.googleapis.com/origin-ci-test/logs/branch-ci-Azure-ARO-HCP-main-e2e-stage-e2e-parallel/12345/finished.json",
		},
		{
			name:    "trailing slash is trimmed",
			viewURL: "https://prow.ci.openshift.org/view/gs/origin-ci-test/logs/some-job/1/",
			want:    "https://storage.googleapis.com/origin-ci-test/logs/some-job/1/finished.json",
		},
		{
			name:    "empty URL is an error",
			viewURL: "",
			wantErr: true,
		},
		{
			name:    "missing /view/gs/ prefix is an error",
			viewURL: "https://prow.ci.openshift.org/something-else/origin-ci-test/logs/some-job/1",
			wantErr: true,
		},
		{
			name:    "empty GCS path after /view/gs/ prefix is an error",
			viewURL: "https://prow.ci.openshift.org/view/gs/",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := finishedJSONURLFromViewURL(tc.viewURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got URL %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGCSBucketAndBuildPath(t *testing.T) {
	tests := []struct {
		name        string
		finishedURL string
		wantBucket  string
		wantPath    string
		wantErr     bool
	}{
		{
			name:        "typical finished.json URL",
			finishedURL: "https://storage.googleapis.com/origin-ci-test/logs/some-job/1/finished.json",
			wantBucket:  "origin-ci-test",
			wantPath:    "logs/some-job/1",
		},
		{
			name:        "not a storage.googleapis.com URL",
			finishedURL: "https://example.com/origin-ci-test/logs/some-job/1/finished.json",
			wantErr:     true,
		},
		{
			name:        "missing build path",
			finishedURL: "https://storage.googleapis.com/finished.json",
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bucket, buildPath, err := gcsBucketAndBuildPath(tc.finishedURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got bucket %q buildPath %q", bucket, buildPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if bucket != tc.wantBucket || buildPath != tc.wantPath {
				t.Fatalf("got bucket %q buildPath %q, want bucket %q buildPath %q", bucket, buildPath, tc.wantBucket, tc.wantPath)
			}
		})
	}
}

// gcsTestServer serves a minimal, in-memory approximation of the two public GCS endpoints
// jobAllowsEV2Retry talks to: the JSON API's objects.list (for listGCSPrefixes, keyed by
// its "prefix" query param) and plain object GETs (for fetchFinishedJSONMetadata, keyed by
// path). It lets tests exercise the full artifacts/ tree walk without hitting real GCS.
func gcsTestServer(t *testing.T, listByPrefix map[string][]string, objectsByPath map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/storage/v1/b/", func(w http.ResponseWriter, r *http.Request) {
		prefix := r.URL.Query().Get("prefix")
		prefixes, ok := listByPrefix[prefix]
		if !ok {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"prefixes":[]}`))
			return
		}
		body, err := json.Marshal(gcsListResponse{Prefixes: prefixes})
		if err != nil {
			t.Fatalf("failed to marshal test list response: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := objectsByPath[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	return httptest.NewServer(mux)
}

// withGCSTestServer points gcsObjectBaseURL/gcsJSONAPIBaseURL at srv for the duration of
// the calling test, restoring the real endpoints on cleanup.
func withGCSTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	origObjectBase, origAPIBase := gcsObjectBaseURL, gcsJSONAPIBaseURL
	gcsObjectBaseURL = srv.URL
	gcsJSONAPIBaseURL = srv.URL + "/storage/v1/b"
	t.Cleanup(func() {
		gcsObjectBaseURL, gcsJSONAPIBaseURL = origObjectBase, origAPIBase
		srv.Close()
	})
}

func TestJobAllowsEV2RetryFindsStepLevelMetadata(t *testing.T) {
	// Mirrors the real ARO-HCP multi-stage layout observed in AROSLSRE-1721: the
	// job-level finished.json's metadata never carries the ev2 keys, but the test
	// step's own finished.json (nested two levels under artifacts/) does.
	const bucket = "test-platform-results"
	const buildPath = "logs/branch-ci-Azure-ARO-HCP-main-e2e-prod-e2e-parallel/12345"

	srv := gcsTestServer(t,
		map[string][]string{
			buildPath + "/artifacts/": {buildPath + "/artifacts/prod-e2e-parallel/"},
			buildPath + "/artifacts/prod-e2e-parallel/": {
				buildPath + "/artifacts/prod-e2e-parallel/aro-hcp-lease-acquire/",
				buildPath + "/artifacts/prod-e2e-parallel/aro-hcp-test-persistent/",
				buildPath + "/artifacts/prod-e2e-parallel/aro-hcp-lease-release/",
			},
		},
		map[string]string{
			"/" + bucket + "/" + buildPath + "/finished.json":                                                     `{"metadata":{"pod":"abc"}}`,
			"/" + bucket + "/" + buildPath + "/artifacts/prod-e2e-parallel/aro-hcp-lease-acquire/finished.json":   `{"metadata":{}}`,
			"/" + bucket + "/" + buildPath + "/artifacts/prod-e2e-parallel/aro-hcp-lease-release/finished.json":   `{"metadata":{}}`,
			"/" + bucket + "/" + buildPath + "/artifacts/prod-e2e-parallel/aro-hcp-test-persistent/finished.json": `{"metadata":{"ev2-failed-tests":["spec A"],"ev2-allow-retry-tests":["spec A"]}}`,
		},
	)
	withGCSTestServer(t, srv)

	viewURL := "https://prow.ci.openshift.org/view/gs/" + bucket + "/" + buildPath
	got, err := jobAllowsEV2Retry(testContext(), viewURL, DefaultMaxEV2AutoRetryFailures)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != KnownIssueEligible {
		t.Fatalf("got %v, want KnownIssueEligible: the step-level finished.json carries a single allow-retry failure", got)
	}
}

func TestJobAllowsEV2RetryNoStepCarriesMetadata(t *testing.T) {
	// If nothing under artifacts/ (nor the job-level finished.json) ever carries
	// ev2FailedTestsKey at all, the aro-hcp-tests step never ran - this is a pre-test
	// infra failure (e.g. lease-acquire capacity exhaustion), not "nothing to retry".
	const bucket = "test-platform-results"
	const buildPath = "logs/some-job/1"

	srv := gcsTestServer(t,
		map[string][]string{
			buildPath + "/artifacts/": {buildPath + "/artifacts/some-workflow/"},
			buildPath + "/artifacts/some-workflow/": {
				buildPath + "/artifacts/some-workflow/some-step/",
			},
		},
		map[string]string{
			"/" + bucket + "/" + buildPath + "/finished.json":                                   `{"metadata":{"pod":"abc"}}`,
			"/" + bucket + "/" + buildPath + "/artifacts/some-workflow/some-step/finished.json": `{"metadata":{}}`,
		},
	)
	withGCSTestServer(t, srv)

	viewURL := "https://prow.ci.openshift.org/view/gs/" + bucket + "/" + buildPath
	got, err := jobAllowsEV2Retry(testContext(), viewURL, DefaultMaxEV2AutoRetryFailures)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != InfraPreconditionEligible {
		t.Fatalf("got %v, want InfraPreconditionEligible: no candidate finished.json carried ev2-failed-tests", got)
	}
}

func TestJobAllowsEV2RetryUsesJobLevelMetadataWhenPresent(t *testing.T) {
	// If some job shape genuinely does aggregate the keys into the job-level
	// finished.json, that's still honored - we don't require the nested layout.
	const bucket = "test-platform-results"
	const buildPath = "logs/some-job/1"

	srv := gcsTestServer(t,
		map[string][]string{buildPath + "/artifacts/": {}},
		map[string]string{
			"/" + bucket + "/" + buildPath + "/finished.json": `{"metadata":{"ev2-failed-tests":[],"ev2-allow-retry-tests":[]}}`,
		},
	)
	withGCSTestServer(t, srv)

	viewURL := "https://prow.ci.openshift.org/view/gs/" + bucket + "/" + buildPath
	got, err := jobAllowsEV2Retry(testContext(), viewURL, DefaultMaxEV2AutoRetryFailures)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != NotEligible {
		t.Fatalf("got %v, want NotEligible: job-level metadata reports a clean run", got)
	}
}

func TestListGCSPrefixesRejectsPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"prefixes":["a/","b/"],"nextPageToken":"more"}`))
	}))
	defer srv.Close()

	origAPIBase := gcsJSONAPIBaseURL
	gcsJSONAPIBaseURL = srv.URL
	defer func() { gcsJSONAPIBaseURL = origAPIBase }()

	_, err := listGCSPrefixes(testContext(), "bucket", "some/prefix/")
	if err == nil {
		t.Fatal("expected an error for a paginated listing, got nil")
	}
}

func TestFetchFinishedJSONAllowsRetry(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       bool
		wantErr    bool
	}{
		{
			name:       "single allow-retry failure qualifies",
			statusCode: http.StatusOK,
			body:       `{"timestamp":1,"passed":false,"result":"FAILURE","metadata":{"ev2-failed-tests":["spec A"],"ev2-allow-retry-tests":["spec A"],"pod":"abc"}}`,
			want:       true,
		},
		{
			name:       "failures at the cap still qualify",
			statusCode: http.StatusOK,
			body:       `{"timestamp":1,"passed":false,"result":"FAILURE","metadata":{"ev2-failed-tests":["spec A","spec B"],"ev2-allow-retry-tests":["spec A","spec B"]}}`,
			want:       true,
		},
		{
			name:       "one failure over the cap disqualifies",
			statusCode: http.StatusOK,
			body:       `{"timestamp":1,"passed":false,"result":"FAILURE","metadata":{"ev2-failed-tests":["spec A","spec B","spec C"],"ev2-allow-retry-tests":["spec A","spec B","spec C"]}}`,
			want:       false,
		},
		{
			name:       "an unlabeled failure disqualifies the whole run",
			statusCode: http.StatusOK,
			body:       `{"timestamp":1,"passed":false,"result":"FAILURE","metadata":{"ev2-failed-tests":["spec A","spec B"],"ev2-allow-retry-tests":["spec A"]}}`,
			want:       false,
		},
		{
			name:       "metadata keys absent means no failures reported",
			statusCode: http.StatusOK,
			body:       `{"timestamp":1,"passed":true,"result":"SUCCESS","metadata":{"pod":"abc"}}`,
			want:       false,
		},
		{
			name:       "empty failed-tests list is a clean run",
			statusCode: http.StatusOK,
			body:       `{"timestamp":1,"passed":true,"result":"SUCCESS","metadata":{"ev2-failed-tests":[],"ev2-allow-retry-tests":[]}}`,
			want:       false,
		},
		{
			name:       "no metadata object at all",
			statusCode: http.StatusOK,
			body:       `{"timestamp":1,"passed":false,"result":"FAILURE"}`,
			want:       false,
		},
		{
			name:       "metadata key wrong type fails closed",
			statusCode: http.StatusOK,
			body:       `{"timestamp":1,"passed":false,"result":"FAILURE","metadata":{"ev2-failed-tests":"spec A"}}`,
			wantErr:    true,
		},
		{
			name:       "metadata list with a non-string element fails closed",
			statusCode: http.StatusOK,
			body:       `{"timestamp":1,"passed":false,"result":"FAILURE","metadata":{"ev2-failed-tests":["spec A", 1]}}`,
			wantErr:    true,
		},
		{
			name:       "non-200 status is an error",
			statusCode: http.StatusNotFound,
			body:       "not found",
			wantErr:    true,
		},
		{
			name:       "invalid JSON is an error",
			statusCode: http.StatusOK,
			body:       `{not json`,
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			got, err := fetchFinishedJSONAllowsRetry(testContext(), srv.URL, DefaultMaxEV2AutoRetryFailures)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFetchFinishedJSONAllowsRetryRejectsOversizedBody(t *testing.T) {
	// A finished.json far larger than the size cap should be rejected outright:
	// fetchFinishedJSONAllowsRetry explicitly errors once the cap is exceeded,
	// before ever attempting to JSON-decode the body - proving the cap is
	// enforced as a hard limit, not just an incidental truncation.
	huge := `{"metadata":{"padding":"` + strings.Repeat("x", maxFinishedJSONBytes+1024) + `","ev2-failed-tests":["spec A"]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	_, err := fetchFinishedJSONAllowsRetry(testContext(), srv.URL, DefaultMaxEV2AutoRetryFailures)
	if err == nil {
		t.Fatal("expected an error from the oversized body, got nil")
	}
}

func TestRetryEligibilityString(t *testing.T) {
	for _, tc := range []struct {
		val  RetryEligibility
		want string
	}{
		{NotEligible, "NotEligible"},
		{KnownIssueEligible, "KnownIssueEligible"},
		{InfraPreconditionEligible, "InfraPreconditionEligible"},
		{RetryEligibility(99), "RetryEligibility(99)"},
	} {
		if got := tc.val.String(); got != tc.want {
			t.Errorf("RetryEligibility(%d).String() = %q, want %q", tc.val, got, tc.want)
		}
	}
}

func TestEV2RetryEligible(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failed     []string
		allowRetry []string
		maxFailure int
		want       bool
	}{
		{
			name:       "clean run does not qualify",
			maxFailure: 2,
			want:       false,
		},
		{
			name:       "single labeled failure qualifies",
			failed:     []string{"spec A"},
			allowRetry: []string{"spec A"},
			maxFailure: 2,
			want:       true,
		},
		{
			name:       "failures at the cap still qualify",
			failed:     []string{"spec A", "spec B"},
			allowRetry: []string{"spec A", "spec B"},
			maxFailure: 2,
			want:       true,
		},
		{
			name:       "one failure over the cap disqualifies",
			failed:     []string{"spec A", "spec B", "spec C"},
			allowRetry: []string{"spec A", "spec B", "spec C"},
			maxFailure: 2,
			want:       false,
		},
		{
			name:       "an unlabeled failure disqualifies the whole run",
			failed:     []string{"spec A", "spec B"},
			allowRetry: []string{"spec A"},
			maxFailure: 2,
			want:       false,
		},
		{
			name:       "a lone unlabeled failure disqualifies",
			failed:     []string{"spec A"},
			allowRetry: nil,
			maxFailure: 2,
			want:       false,
		},
		{
			name:       "a lower configured cap tightens eligibility",
			failed:     []string{"spec A", "spec B"},
			allowRetry: []string{"spec A", "spec B"},
			maxFailure: 1,
			want:       false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ev2RetryEligible(tc.failed, tc.allowRetry, tc.maxFailure); got != tc.want {
				t.Fatalf("ev2RetryEligible(%v, %v, %d) = %v, want %v", tc.failed, tc.allowRetry, tc.maxFailure, got, tc.want)
			}
		})
	}
}
