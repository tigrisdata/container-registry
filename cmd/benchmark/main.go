package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
)

const (
	defaultRegistry   = "http://localhost:5000"
	defaultRepository = "benchmark/test"
	defaultSizes      = "1GB,2GB"
	defaultIterations = 3
	defaultOutput     = "text"
)

// BenchmarkResult holds the results of a single benchmark run
type BenchmarkResult struct {
	Size       int64         `json:"size_bytes"`
	Duration   time.Duration `json:"duration_ns"`
	Throughput float64       `json:"throughput_mbps"`
}

// SizeResults holds aggregated results for a specific size
type SizeResults struct {
	SizeBytes       int64     `json:"size_bytes"`
	SizeHuman       string    `json:"size_human"`
	Iterations      int       `json:"iterations"`
	MeanThroughput  float64   `json:"mean_throughput_mbps"`
	StdDevThroughput float64  `json:"std_dev_mbps"`
	MinThroughput   float64   `json:"min_throughput_mbps"`
	MaxThroughput   float64   `json:"max_throughput_mbps"`
	Durations       []int64   `json:"durations_ms"`
}

// BenchmarkOutput is the full output structure for JSON
type BenchmarkOutput struct {
	Registry    string        `json:"registry"`
	Repository  string        `json:"repository"`
	Timestamp   string        `json:"timestamp"`
	PushResults []SizeResults `json:"push_results"`
	PullResults []SizeResults `json:"pull_results"`
}

func main() {
	// Parse command line flags
	registry := flag.String("registry", defaultRegistry, "Target registry URL")
	repository := flag.String("repository", defaultRepository, "Repository path for test images")
	sizes := flag.String("sizes", defaultSizes, "Comma-separated blob sizes to test (e.g., 1GB,2GB)")
	iterations := flag.Int("iterations", defaultIterations, "Number of iterations per size")
	output := flag.String("output", defaultOutput, "Output format: text or json")
	flag.Parse()

	// Parse sizes
	sizeList, err := parseSizes(*sizes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing sizes: %v\n", err)
		os.Exit(1)
	}

	// Validate output format
	if *output != "text" && *output != "json" {
		fmt.Fprintf(os.Stderr, "Invalid output format: %s (must be 'text' or 'json')\n", *output)
		os.Exit(1)
	}

	// Run benchmarks
	ctx := context.Background()
	benchmarkOutput := BenchmarkOutput{
		Registry:   *registry,
		Repository: *repository,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	fmt.Fprintf(os.Stderr, "Container Registry Benchmark\n")
	fmt.Fprintf(os.Stderr, "============================\n")
	fmt.Fprintf(os.Stderr, "Registry: %s\n", *registry)
	fmt.Fprintf(os.Stderr, "Repository: %s\n", *repository)
	fmt.Fprintf(os.Stderr, "Sizes: %v\n", sizeList)
	fmt.Fprintf(os.Stderr, "Iterations: %d\n\n", *iterations)

	for _, size := range sizeList {
		sizeHuman := humanizeBytes(size)
		fmt.Fprintf(os.Stderr, "Testing %s blobs...\n", sizeHuman)

		// Generate blob once per size
		fmt.Fprintf(os.Stderr, "  Generating %s random blob...\n", sizeHuman)
		data, dgst := generateBlob(size)
		fmt.Fprintf(os.Stderr, "  Blob digest: %s\n", dgst)

		// Push benchmarks
		// We run all push iterations, deleting between each except the last one
		// The last pushed blob is reused for pull benchmarks
		fmt.Fprintf(os.Stderr, "  Running push benchmarks...\n")
		pushResults := make([]BenchmarkResult, 0, *iterations)
		for i := 0; i < *iterations; i++ {
			result, err := benchmarkPush(ctx, *registry, *repository, data, dgst, i)
			if err != nil {
				fmt.Fprintf(os.Stderr, "    Push iteration %d failed: %v\n", i+1, err)
				continue
			}
			pushResults = append(pushResults, result)
			fmt.Fprintf(os.Stderr, "    Push %d: %.2f MB/s (%.2fs)\n", i+1, result.Throughput, result.Duration.Seconds())

			// Delete after each push except the last one (reuse for pull benchmarks)
			if i < *iterations-1 {
				if err := deleteBlob(ctx, *registry, *repository, dgst); err != nil {
					fmt.Fprintf(os.Stderr, "    Warning: cleanup failed: %v\n", err)
				}
			}
		}

		// The last pushed blob is already in the registry for pull benchmarks
		fmt.Fprintf(os.Stderr, "  Reusing last pushed blob for pull benchmarks...\n")

		// Pull benchmarks
		fmt.Fprintf(os.Stderr, "  Running pull benchmarks...\n")
		pullResults := make([]BenchmarkResult, 0, *iterations)
		for i := 0; i < *iterations; i++ {
			result, err := benchmarkPull(ctx, *registry, *repository, dgst, size)
			if err != nil {
				fmt.Fprintf(os.Stderr, "    Pull iteration %d failed: %v\n", i+1, err)
				continue
			}
			pullResults = append(pullResults, result)
			fmt.Fprintf(os.Stderr, "    Pull %d: %.2f MB/s (%.2fs)\n", i+1, result.Throughput, result.Duration.Seconds())
		}

		// Cleanup
		fmt.Fprintf(os.Stderr, "  Cleaning up...\n")
		if err := deleteBlob(ctx, *registry, *repository, dgst); err != nil {
			fmt.Fprintf(os.Stderr, "    Warning: cleanup failed: %v\n", err)
		}

		// Aggregate results
		if len(pushResults) > 0 {
			benchmarkOutput.PushResults = append(benchmarkOutput.PushResults, aggregateResults(size, pushResults))
		}
		if len(pullResults) > 0 {
			benchmarkOutput.PullResults = append(benchmarkOutput.PullResults, aggregateResults(size, pullResults))
		}

		fmt.Fprintf(os.Stderr, "\n")
	}

	// Output results
	if *output == "json" {
		outputJSON(benchmarkOutput)
	} else {
		outputText(benchmarkOutput)
	}
}

// parseSizes parses a comma-separated list of sizes like "1GB,2GB" into bytes
func parseSizes(s string) ([]int64, error) {
	parts := strings.Split(s, ",")
	sizes := make([]int64, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.ToUpper(part)

		var multiplier int64 = 1
		var numStr string

		if strings.HasSuffix(part, "GB") {
			multiplier = 1024 * 1024 * 1024
			numStr = strings.TrimSuffix(part, "GB")
		} else if strings.HasSuffix(part, "MB") {
			multiplier = 1024 * 1024
			numStr = strings.TrimSuffix(part, "MB")
		} else if strings.HasSuffix(part, "KB") {
			multiplier = 1024
			numStr = strings.TrimSuffix(part, "KB")
		} else {
			// Assume bytes
			numStr = part
		}

		num, err := strconv.ParseInt(numStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid size '%s': %w", part, err)
		}

		sizes = append(sizes, num*multiplier)
	}

	return sizes, nil
}

// humanizeBytes converts bytes to human-readable format
func humanizeBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// generateBlob generates a random blob of the specified size using ChaCha8 RNG
func generateBlob(size int64) ([]byte, digest.Digest) {
	// Use time-based seed for randomness
	seed := [32]byte{}
	seedVal := time.Now().UnixNano()
	for i := 0; i < 8; i++ {
		seed[i] = byte(seedVal >> (i * 8))
	}

	rng := rand.NewChaCha8(seed)
	data := make([]byte, size)
	_, _ = rng.Read(data)

	dgst := digest.FromBytes(data)
	return data, dgst
}

// benchmarkPush benchmarks pushing a blob to the registry
func benchmarkPush(ctx context.Context, registry, repository string, data []byte, dgst digest.Digest, iteration int) (BenchmarkResult, error) {
	client := &http.Client{
		Timeout: 30 * time.Minute, // Large timeout for big blobs
	}

	// Step 1: Initiate upload (POST)
	uploadURL := fmt.Sprintf("%s/v2/%s/blobs/uploads/", registry, repository)
	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, nil)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("creating POST request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("initiating upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return BenchmarkResult{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	// Get the upload location
	location := resp.Header.Get("Location")
	if location == "" {
		return BenchmarkResult{}, fmt.Errorf("no Location header in response")
	}

	// Make location absolute if needed
	if !strings.HasPrefix(location, "http") {
		location = registry + location
	}

	// Step 2: Upload blob (PUT) - monolithic upload
	// Add digest query parameter
	if strings.Contains(location, "?") {
		location = location + "&digest=" + dgst.String()
	} else {
		location = location + "?digest=" + dgst.String()
	}

	// Start timing the actual upload
	start := time.Now()

	req, err = http.NewRequestWithContext(ctx, "PUT", location, bytes.NewReader(data))
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("creating PUT request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", strconv.FormatInt(int64(len(data)), 10))

	resp, err = client.Do(req)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("uploading blob: %w", err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return BenchmarkResult{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	throughput := float64(len(data)) / duration.Seconds() / (1024 * 1024)

	return BenchmarkResult{
		Size:       int64(len(data)),
		Duration:   duration,
		Throughput: throughput,
	}, nil
}

// benchmarkPull benchmarks pulling a blob from the registry
func benchmarkPull(ctx context.Context, registry, repository string, dgst digest.Digest, expectedSize int64) (BenchmarkResult, error) {
	client := &http.Client{
		Timeout: 30 * time.Minute,
	}

	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", registry, repository, dgst.String())

	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, "GET", blobURL, nil)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("creating GET request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("fetching blob: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return BenchmarkResult{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	// Read and discard the body
	written, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("reading body: %w", err)
	}

	duration := time.Since(start)
	throughput := float64(written) / duration.Seconds() / (1024 * 1024)

	return BenchmarkResult{
		Size:       written,
		Duration:   duration,
		Throughput: throughput,
	}, nil
}

// deleteBlob deletes a blob from the registry
func deleteBlob(ctx context.Context, registry, repository string, dgst digest.Digest) error {
	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", registry, repository, dgst.String())

	req, err := http.NewRequestWithContext(ctx, "DELETE", blobURL, nil)
	if err != nil {
		return fmt.Errorf("creating DELETE request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("deleting blob: %w", err)
	}
	defer resp.Body.Close()

	// Accept 202 (Accepted) or 404 (Not Found - already deleted)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// aggregateResults aggregates multiple benchmark results into statistics
func aggregateResults(size int64, results []BenchmarkResult) SizeResults {
	if len(results) == 0 {
		return SizeResults{SizeBytes: size, SizeHuman: humanizeBytes(size)}
	}

	throughputs := make([]float64, len(results))
	durations := make([]int64, len(results))
	var sum float64
	minT := results[0].Throughput
	maxT := results[0].Throughput

	for i, r := range results {
		throughputs[i] = r.Throughput
		durations[i] = r.Duration.Milliseconds()
		sum += r.Throughput
		if r.Throughput < minT {
			minT = r.Throughput
		}
		if r.Throughput > maxT {
			maxT = r.Throughput
		}
	}

	mean := sum / float64(len(results))

	// Calculate standard deviation
	var variance float64
	for _, t := range throughputs {
		variance += (t - mean) * (t - mean)
	}
	variance /= float64(len(results))
	stdDev := math.Sqrt(variance)

	return SizeResults{
		SizeBytes:        size,
		SizeHuman:        humanizeBytes(size),
		Iterations:       len(results),
		MeanThroughput:   mean,
		StdDevThroughput: stdDev,
		MinThroughput:    minT,
		MaxThroughput:    maxT,
		Durations:        durations,
	}
}

// outputJSON outputs results in JSON format
func outputJSON(output BenchmarkOutput) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(output)
}

// outputText outputs results in human-readable text format
func outputText(output BenchmarkOutput) {
	fmt.Println()
	fmt.Println("Container Registry Benchmark Results")
	fmt.Println("=====================================")
	fmt.Printf("Registry: %s\n", output.Registry)
	fmt.Printf("Repository: %s\n", output.Repository)
	fmt.Printf("Timestamp: %s\n", output.Timestamp)
	fmt.Println()

	if len(output.PushResults) > 0 {
		fmt.Println("PUSH RESULTS")
		fmt.Println("+--------+------------+-----------------+--------------+")
		fmt.Println("|  Size  | Iterations |   Throughput    |   Std Dev    |")
		fmt.Println("+--------+------------+-----------------+--------------+")
		for _, r := range output.PushResults {
			fmt.Printf("| %6s | %10d | %10.2f MB/s | %8.2f MB/s |\n",
				r.SizeHuman, r.Iterations, r.MeanThroughput, r.StdDevThroughput)
		}
		fmt.Println("+--------+------------+-----------------+--------------+")
		fmt.Println()
	}

	if len(output.PullResults) > 0 {
		fmt.Println("PULL RESULTS")
		fmt.Println("+--------+------------+-----------------+--------------+")
		fmt.Println("|  Size  | Iterations |   Throughput    |   Std Dev    |")
		fmt.Println("+--------+------------+-----------------+--------------+")
		for _, r := range output.PullResults {
			fmt.Printf("| %6s | %10d | %10.2f MB/s | %8.2f MB/s |\n",
				r.SizeHuman, r.Iterations, r.MeanThroughput, r.StdDevThroughput)
		}
		fmt.Println("+--------+------------+-----------------+--------------+")
	}
}
