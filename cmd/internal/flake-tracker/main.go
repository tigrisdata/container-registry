package main

// Flaky test tracker
//
// This command runs in CI after tests. It reads the current pipeline's test
// report via the GitLab REST API, deterministically identifies failing test
// cases, and for each unique test creates or updates a tracking issue in
// GitLab. Each failure occurrence is appended as a comment, and a bucketed
// occurrence label is applied to support prioritization. When a new issue is
// created, a Slack notification is sent (if a webhook is provided).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	slack "github.com/docker/distribution/cmd/internal/release-cli/slack"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

type testReport struct {
	TestSuites []struct {
		Name      string `json:"name"`
		Failed    int    `json:"failed"`
		Error     int    `json:"error"`
		TestCases []struct {
			Name         string          `json:"name"`
			Classname    string          `json:"classname"`
			File         string          `json:"file"`
			Status       string          `json:"status"`
			SystemOutput json.RawMessage `json:"system_output"`
			StackTrace   json.RawMessage `json:"stack_trace"`
		} `json:"test_cases"`
	} `json:"test_suites"`
}

func main() {
	token := os.Getenv("FLAKE_TRACKER_GL_TOKEN")
	if token == "" {
		logf("no API token provided (FLAKE_TRACKER_GL_TOKEN), exiting")
		os.Exit(1)
	}

	// Core environment / CI variables
	baseURL := getEnvOrDefault("CI_SERVER_URL", "https://gitlab.com")
	apiV4 := getEnvOrDefault("CI_API_V4_URL", baseURL+"/api/v4")
	projectID := mustEnv("CI_PROJECT_ID")
	// The project to create/update flaky issues in. If not provided, default to
	// the current CI project. This should be a numeric project ID.
	issueProjectID := os.Getenv("FLAKE_TRACKER_ISSUE_PROJECT_ID")
	if issueProjectID == "" {
		issueProjectID = projectID
	}
	issuePID, err := strconv.Atoi(issueProjectID)
	if err != nil {
		logf("invalid project id: %v", err)
		os.Exit(1)
	}
	pipelineID := mustEnv("CI_PIPELINE_ID")
	projectURL := os.Getenv("CI_PROJECT_URL")
	// URL used in comments/notifications for quick navigation
	pipelineURL := fmt.Sprintf("%s/-/pipelines/%s", projectURL, pipelineID)
	sha := os.Getenv("CI_COMMIT_SHA")
	ref := os.Getenv("CI_COMMIT_REF_NAME")

	logf("fetching pipeline test report: %s/projects/%s/pipelines/%s/test_report (auth=PRIVATE-TOKEN)", apiV4, projectID, pipelineID)
	failures, err := fetchFailuresREST(apiV4, projectID, pipelineID, token)
	if err != nil {
		logf("unable to fetch test report: %v", err)
		os.Exit(1)
	}
	if len(failures) == 0 {
		logf("no failures found, exiting")
		return
	}

	// GitLab API client (official client-go)
	cli, err := gitlab.NewClient(token)
	if err != nil {
		logf("failed to init gitlab client: %v", err)
		os.Exit(1)
	}

	created := 0
	updated := 0
	for _, c := range failures {
		testID := normalizeTestID(c["classname"], c["name"])
		title := "[Flaky] " + testID
		file := c["file"]

		// Ensure we do not create duplicates: find an issue with exact title
		issue, err := findIssueByExactTitle(cli, issuePID, title)
		if err != nil {
			logf("search issues failed for %q: %v", title, err)
			continue
		}

		now := time.Now().UTC()
		shortSHA := sha
		if len(shortSHA) > 8 {
			shortSHA = shortSHA[:8]
		}
		// Deterministic occurrence marker used for counting (keep the exact prefix)
		occurrence := fmt.Sprintf(
			"FLAKE_OCCURRENCE:\n"+
				"- Test: `%s`\n"+
				"- File: `%s`\n"+
				"- Ref: `%s` @ `%s`\n"+
				"- Pipeline: `%s` ([view](%s))\n"+
				"- Time: %s",
			testID, file, ref, shortSHA, pipelineID, pipelineURL, now.Format(time.RFC3339),
		)
		if snip := strings.TrimSpace(c["snippet"]); snip != "" {
			occurrence += "\n- Failure snippet:\n```\n" + snip + "\n```"
		}

		// Base labels identifying the automation and category
		baseLabels := []string{
			"flaky::test",
			"flaky::auto",
			"Category:Container Registry",
			"backend",
			"devops::package",
			"failure::flaky-test",
			"golang",
			"group::container registry",
			"maintenance::pipelines",
			"section::ci",
			"type::maintenance",
		}

		if issue == nil {
			desc := fmt.Sprintf(
				"This issue was created automatically to track occurrences of a flaky test.\n\n"+
					"- Test: `%s`\n"+
					"- File: `%s`\n"+
					"- Ref: `%s` @ `%s`\n"+
					"- Pipeline: `%s` ([view](%s))\n"+
					"- Time: %s\n\n"+
					"Occurrences will be added as comments with details.\n",
				testID, file, ref, func() string {
					if len(sha) > 8 {
						return sha[:8]
					}
					return sha
				}(), pipelineID, pipelineURL, now.Format(time.RFC3339),
			)
			if snip := strings.TrimSpace(c["snippet"]); snip != "" {
				desc += "\n- Failure snippet:\n```\n" + snip + "\n```\n"
			}
			baseLabels = append(baseLabels, bucketForCount(1))
			// Create a new issue initialized with the first bucket label
			lo := gitlab.LabelOptions(baseLabels)
			cio := &gitlab.CreateIssueOptions{
				Title:       gitlab.Ptr(title),
				Description: gitlab.Ptr(desc),
				Labels:      &lo,
				Weight:      gitlab.Ptr(1),
			}
			is, _, err := cli.Issues.CreateIssue(issuePID, cio)
			if err != nil {
				logf("create issue failed for %q: %v", title, err)
				continue
			}
			if _, _, err := cli.Notes.CreateIssueNote(issuePID, is.IID, &gitlab.CreateIssueNoteOptions{Body: gitlab.Ptr(occurrence)}); err != nil {
				logf("failed to add occurrence note to issue #%d: %v", is.IID, err)
			}
			created++
			logf("created issue #%d for %s", is.IID, testID)
			// Slack notification (best-effort, non-blocking)
			if webhook := os.Getenv("FLAKE_TRACKER_SLACK_WEBHOOK_URL"); webhook != "" {
				msg := fmt.Sprintf(
					":warning: Flaky test detected: `%s`\nIssue: %s\nPipeline: %s",
					testID, is.WebURL, pipelineURL,
				)
				if err := slack.SendSlackNotification(webhook, msg); err != nil {
					logf("slack notify failed: %v", err)
				}
			}
			continue
		}

		// If the matching issue is closed, reopen it to make it visible again
		if strings.EqualFold(issue.State, "closed") {
			if _, _, err := cli.Issues.UpdateIssue(issuePID, issue.IID, &gitlab.UpdateIssueOptions{
				StateEvent: gitlab.Ptr("reopen"),
			}); err != nil {
				logf("failed to reopen issue #%d: %v", issue.IID, err)
			}
		}

		prev, err := countOccurrences(cli, issuePID, issue.IID)
		if err != nil {
			logf("counting occurrences failed for !%d: %v", issue.IID, err)
			continue
		}
		newCount := prev + 1
		// Replace any previous occurrence bucket with the current total bucket
		labels := withoutOccurrenceBuckets(issue.Labels)
		labels = append(labels, bucketForCount(newCount))
		ulo := gitlab.LabelOptions(labels)
		if _, _, err := cli.Issues.UpdateIssue(issuePID, issue.IID, &gitlab.UpdateIssueOptions{Labels: &ulo, Weight: gitlab.Ptr(newCount)}); err != nil {
			logf("failed to update labels for issue #%d: %v", issue.IID, err)
		}
		if _, _, err := cli.Notes.CreateIssueNote(issuePID, issue.IID, &gitlab.CreateIssueNoteOptions{Body: gitlab.Ptr(occurrence)}); err != nil {
			logf("failed to add occurrence note to issue #%d: %v", issue.IID, err)
		}
		updated++
		logf("updated issue #%d for %s (occurrences=%d)", issue.IID, testID, newCount)
	}

	logf("done. created=%d updated=%d", created, updated)
}

// getEnvOrDefault returns the environment variable value if set, otherwise def.
func getEnvOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// mustEnv returns the env var value or panics if unset. Used for core CI vars
// that must be present for the command to function.
func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		panic(fmt.Sprintf("missing required env var: %s", name))
	}
	return v
}

// logf emits a namespaced log line to stdout.
func logf(format string, args ...any) {
	fmt.Printf("[flaky-tracker] "+format+"\n", args...)
}

// fatalf logs the message and exits the program with a non-zero status code.
// (removed) deep-exit: avoid os.Exit outside main/init

// normalizeTestID returns a stable identifier for a test case, combining its
// classname (package or suite) with the test name when available.
func normalizeTestID(classname, name string) string {
	classname = strings.TrimSpace(classname)
	name = strings.TrimSpace(name)
	if classname != "" {
		return classname + "." + name
	}
	return name
}

// bucketForCount maps an occurrence count to a label used for triage buckets.
func bucketForCount(n int) string {
	switch {
	case n <= 3:
		return "flaky-occurrences::1-3"
	case n <= 10:
		return "flaky-occurrences::4-10"
	case n <= 25:
		return "flaky-occurrences::11-25"
	default:
		return "flaky-occurrences::>25"
	}
}

// fetchFailuresREST loads the pipeline test report and extracts failed/error
// cases. Returns a minimal representation of failing cases.
func fetchFailuresREST(apiV4, projectID, pipelineID, token string) ([]map[string]string, error) {
	url := fmt.Sprintf("%s/projects/%s/pipelines/%s/test_report", apiV4, projectID, pipelineID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var tr testReport
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}

	// Extract failures from the test report
	// We are interested in the test cases that have a status of "failed" or "error".
	// We also want to extract the system output and stack trace from the test case.
	// We will then use the system output and stack trace to create a snippet of the failure.
	var failures []map[string]string
	for _, s := range tr.TestSuites {
		for _, c := range s.TestCases {
			st := strings.ToLower(strings.TrimSpace(c.Status))
			if st == "failed" || st == "error" {
				snippet := strings.TrimSpace(extractTextField(c.SystemOutput))
				if snippet == "" {
					snippet = strings.TrimSpace(extractTextField(c.StackTrace))
				}
				if snippet != "" {
					snippet = trimSnippet(snippet, 2000)
				}
				failures = append(failures, map[string]string{
					"name":      c.Name,
					"classname": c.Classname,
					"file":      c.File,
					"snippet":   snippet,
				})
			}
		}
	}
	return failures, nil
}

// findIssueByExactTitle searches issues by title in the target project and
// returns an exact-match issue if found (opened first, then closed).
func findIssueByExactTitle(cli *gitlab.Client, projectID int, title string) (*gitlab.Issue, error) {
	if is, err := searchTitleByState(cli, projectID, title, "opened"); err != nil || is != nil {
		return is, err
	}
	return searchTitleByState(cli, projectID, title, "closed")
}

// searchTitleByState searches issues by title in the target project and returns an exact-match issue if found.
func searchTitleByState(cli *gitlab.Client, projectID int, title, state string) (*gitlab.Issue, error) {
	page := 1
	for {
		issues, resp, err := cli.Issues.ListProjectIssues(projectID, &gitlab.ListProjectIssuesOptions{
			Search: gitlab.Ptr(title),
			ListOptions: gitlab.ListOptions{
				PerPage: 100,
				Page:    page,
			},
			State: gitlab.Ptr(state),
		})
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if is.Title == title {
				return is, nil
			}
		}
		if resp.CurrentPage >= resp.TotalPages || len(issues) == 0 {
			return nil, nil
		}
		page++
	}
}

// countOccurrences counts how many tracker comments were previously added to
// the issue by scanning for the "FLAKE_OCCURRENCE:" marker.
func countOccurrences(cli *gitlab.Client, projectID, issueIID int) (int, error) {
	page := 1
	total := 0
	for {
		notes, resp, err := cli.Notes.ListIssueNotes(projectID, issueIID, &gitlab.ListIssueNotesOptions{
			ListOptions: gitlab.ListOptions{PerPage: 100, Page: page},
		})
		if err != nil {
			return 0, err
		}
		for _, n := range notes {
			if strings.Contains(n.Body, "FLAKE_OCCURRENCE:") {
				total++
			}
		}
		if resp.CurrentPage >= resp.TotalPages || len(notes) == 0 {
			break
		}
		page++
	}
	return total, nil
}

// withoutOccurrenceBuckets removes any existing occurrence bucket labels to
// allow applying the correct bucket for the latest total count.
func withoutOccurrenceBuckets(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if !strings.HasPrefix(l, "flaky-occurrences::") {
			out = append(out, l)
		}
	}
	return out
}

// trimSnippet returns at most max characters from s, preserving whole lines when possible.
// If truncated, it appends an ellipsis marker to indicate omission.
func trimSnippet(s string, maxChar int) string {
	if len(s) <= maxChar {
		return s
	}
	// Try to cut at a newline boundary before max
	cutoff := maxChar
	if idx := strings.LastIndex(s[:maxChar], "\n"); idx > 0 {
		cutoff = idx
	}
	return s[:cutoff] + "\n... (truncated)"
}

// extractTextField tries to read a human-readable string from a JSON value that
// may either be a plain string or an object containing common text fields.
func extractTextField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Case 1: plain string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Case 2: object with possible text fields
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		// Prefer stdout/stderr if present
		var parts []string
		if v, ok := obj["stdout"].(string); ok && strings.TrimSpace(v) != "" {
			parts = append(parts, v)
		}
		if v, ok := obj["stderr"].(string); ok && strings.TrimSpace(v) != "" {
			parts = append(parts, v)
		}
		if len(parts) > 0 {
			return strings.TrimSpace(strings.Join(parts, "\n"))
		}
		// Fall back to common singular fields
		for _, k := range []string{"value", "message", "content", "trace", "error", "text"} {
			if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}
