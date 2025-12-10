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
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
)

const (
	defaultRegistry   = "http://localhost:5000"
	defaultRepository = "benchmark/test"
	defaultSizes      = "1GB,2GB"
	defaultIterations = 3
	defaultOutput     = "text"
	defaultChunkSize  = 5 * 1024 * 1024 // 5MB default chunk size (matches Docker client)
	defaultLayers     = 1
	defaultTag        = "benchmark"
	defaultConfigSize = 1024 // 1KB config blob

	// Docker Schema v2 media types
	mediaTypeManifest    = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeImageConfig = "application/vnd.docker.container.image.v1+json"
	mediaTypeLayer       = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

// BenchmarkResult holds the results of a single benchmark run
type BenchmarkResult struct {
	Size       int64         `json:"size_bytes"`
	Duration   time.Duration `json:"duration_ns"`
	Throughput float64       `json:"throughput_mbps"`
}

// SizeResults holds aggregated results for a specific size
type SizeResults struct {
	SizeBytes        int64   `json:"size_bytes"`
	SizeHuman        string  `json:"size_human"`
	Iterations       int     `json:"iterations"`
	MeanThroughput   float64 `json:"mean_throughput_mbps"`
	StdDevThroughput float64 `json:"std_dev_mbps"`
	MinThroughput    float64 `json:"min_throughput_mbps"`
	MaxThroughput    float64 `json:"max_throughput_mbps"`
	Durations        []int64 `json:"durations_ms"`
}

// BenchmarkOutput is the full output structure for JSON
type BenchmarkOutput struct {
	Registry    string        `json:"registry"`
	Repository  string        `json:"repository"`
	Timestamp   string        `json:"timestamp"`
	PushResults []SizeResults `json:"push_results"`
	PullResults []SizeResults `json:"pull_results"`
}

// FullImageResult holds detailed results for full-image benchmarks
type FullImageResult struct {
	// Push metrics
	ConfigPushDuration   time.Duration `json:"config_push_duration_ns"`
	LayerPushDurations   []int64       `json:"layer_push_durations_ms"`
	ManifestPushDuration time.Duration `json:"manifest_push_duration_ns"`
	TotalPushDuration    time.Duration `json:"total_push_duration_ns"`
	PushThroughputMBps   float64       `json:"push_throughput_mbps"`

	// Pull metrics
	ManifestPullDuration time.Duration `json:"manifest_pull_duration_ns"`
	ConfigPullDuration   time.Duration `json:"config_pull_duration_ns"`
	LayerPullDurations   []int64       `json:"layer_pull_durations_ms"`
	TotalPullDuration    time.Duration `json:"total_pull_duration_ns"`
	PullThroughputMBps   float64       `json:"pull_throughput_mbps"`

	// Metadata
	TotalBytes    int64 `json:"total_bytes"`
	NumLayers     int   `json:"num_layers"`
	ChunkedUpload bool  `json:"chunked_upload"`
	ChunkSize     int   `json:"chunk_size"`
}

// Manifest represents a Docker Schema v2 manifest for benchmarking
type benchmarkManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	MediaType     string                `json:"mediaType"`
	Config        descriptorBenchmark   `json:"config"`
	Layers        []descriptorBenchmark `json:"layers"`
}

// descriptorBenchmark represents a content descriptor
type descriptorBenchmark struct {
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest"`
}

func main() {
	// Parse command line flags
	registry := flag.String("registry", defaultRegistry, "Target registry URL")
	repository := flag.String("repository", defaultRepository, "Repository path for test images")
	sizes := flag.String("sizes", defaultSizes, "Comma-separated layer sizes to test (e.g., 1GB,2GB)")
	iterations := flag.Int("iterations", defaultIterations, "Number of iterations per size")
	output := flag.String("output", defaultOutput, "Output format: text or json")

	// Benchmark mode flags
	monolithic := flag.Bool("monolithic", false, "Use monolithic uploads instead of chunked (default: chunked)")
	chunkSize := flag.Int("chunk-size", defaultChunkSize, "Chunk size in bytes for chunked uploads (default: 5MB)")
	layers := flag.Int("layers", defaultLayers, "Number of layers per image")
	tag := flag.String("tag", defaultTag, "Tag for manifest")

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

	useChunked := !*monolithic

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
	fmt.Fprintf(os.Stderr, "Layers: %d\n", *layers)
	fmt.Fprintf(os.Stderr, "Tag: %s\n", *tag)
	if useChunked {
		fmt.Fprintf(os.Stderr, "Upload: chunked (%s chunks)\n", humanizeBytes(int64(*chunkSize)))
	} else {
		fmt.Fprintf(os.Stderr, "Upload: monolithic\n")
	}
	fmt.Fprintf(os.Stderr, "Sizes: %v\n", sizeList)
	fmt.Fprintf(os.Stderr, "Iterations: %d\n\n", *iterations)

	runFullImageBenchmarks(ctx, *registry, *repository, *tag, sizeList, *layers, *iterations, useChunked, *chunkSize, &benchmarkOutput)

	// Output results
	if *output == "json" {
		outputJSON(benchmarkOutput)
	} else {
		outputText(benchmarkOutput)
	}
}

// runFullImageBenchmarks runs full-image benchmarks (config + layers + manifest)
func runFullImageBenchmarks(ctx context.Context, registry, repository, tag string, sizeList []int64, numLayers, iterations int, useChunked bool, chunkSize int, benchmarkOutput *BenchmarkOutput) {
	for _, layerSize := range sizeList {
		sizeHuman := humanizeBytes(layerSize)
		totalSize := int64(defaultConfigSize) + layerSize*int64(numLayers)
		totalHuman := humanizeBytes(totalSize)

		fmt.Fprintf(os.Stderr, "Testing full image with %d x %s layers (total: %s)...\n", numLayers, sizeHuman, totalHuman)

		// Generate config blob
		fmt.Fprintf(os.Stderr, "  Generating config blob (%s)...\n", humanizeBytes(defaultConfigSize))
		configData, configDigest := generateBlob(defaultConfigSize)

		// Generate layer blobs
		layerData := make([][]byte, numLayers)
		layerDigests := make([]digest.Digest, numLayers)
		for i := 0; i < numLayers; i++ {
			fmt.Fprintf(os.Stderr, "  Generating layer %d (%s)...\n", i+1, sizeHuman)
			layerData[i], layerDigests[i] = generateBlob(layerSize)
		}

		// Push benchmarks
		fmt.Fprintf(os.Stderr, "  Running push benchmarks...\n")
		pushResults := make([]BenchmarkResult, 0, iterations)
		for i := 0; i < iterations; i++ {
			// Use unique tag per iteration to avoid conflicts
			iterTag := fmt.Sprintf("%s-%d", tag, i)

			result, err := benchmarkFullImagePush(ctx, registry, repository, iterTag, configData, configDigest, layerData, layerDigests, useChunked, chunkSize)
			if err != nil {
				fmt.Fprintf(os.Stderr, "    Push iteration %d failed: %v\n", i+1, err)
				continue
			}

			// Convert to BenchmarkResult for aggregation
			pushResults = append(pushResults, BenchmarkResult{
				Size:       result.TotalBytes,
				Duration:   result.TotalPushDuration,
				Throughput: result.PushThroughputMBps,
			})
			fmt.Fprintf(os.Stderr, "    Push %d: %.2f MB/s (%.2fs) [manifest: %dms]\n",
				i+1, result.PushThroughputMBps, result.TotalPushDuration.Seconds(),
				result.ManifestPushDuration.Milliseconds())

			// Delete manifest and blobs after each push except the last one
			if i < iterations-1 {
				if err := deleteManifest(ctx, registry, repository, iterTag); err != nil {
					fmt.Fprintf(os.Stderr, "    Warning: manifest cleanup failed: %v\n", err)
				}
				if err := deleteBlob(ctx, registry, repository, configDigest); err != nil {
					fmt.Fprintf(os.Stderr, "    Warning: config cleanup failed: %v\n", err)
				}
				for j, dgst := range layerDigests {
					if err := deleteBlob(ctx, registry, repository, dgst); err != nil {
						fmt.Fprintf(os.Stderr, "    Warning: layer %d cleanup failed: %v\n", j, err)
					}
				}
			}
		}

		// Pull benchmarks - use the last pushed image
		lastTag := fmt.Sprintf("%s-%d", tag, iterations-1)
		fmt.Fprintf(os.Stderr, "  Running pull benchmarks...\n")
		pullResults := make([]BenchmarkResult, 0, iterations)
		for i := 0; i < iterations; i++ {
			result, err := benchmarkFullImagePull(ctx, registry, repository, lastTag, totalSize)
			if err != nil {
				fmt.Fprintf(os.Stderr, "    Pull iteration %d failed: %v\n", i+1, err)
				continue
			}

			// Convert to BenchmarkResult for aggregation
			pullResults = append(pullResults, BenchmarkResult{
				Size:       result.TotalBytes,
				Duration:   result.TotalPullDuration,
				Throughput: result.PullThroughputMBps,
			})
			fmt.Fprintf(os.Stderr, "    Pull %d: %.2f MB/s (%.2fs) [manifest: %dms]\n",
				i+1, result.PullThroughputMBps, result.TotalPullDuration.Seconds(),
				result.ManifestPullDuration.Milliseconds())
		}

		// Cleanup
		fmt.Fprintf(os.Stderr, "  Cleaning up...\n")
		if err := deleteManifest(ctx, registry, repository, lastTag); err != nil {
			fmt.Fprintf(os.Stderr, "    Warning: manifest cleanup failed: %v\n", err)
		}
		if err := deleteBlob(ctx, registry, repository, configDigest); err != nil {
			fmt.Fprintf(os.Stderr, "    Warning: config cleanup failed: %v\n", err)
		}
		for j, dgst := range layerDigests {
			if err := deleteBlob(ctx, registry, repository, dgst); err != nil {
				fmt.Fprintf(os.Stderr, "    Warning: layer %d cleanup failed: %v\n", j, err)
			}
		}

		// Aggregate results
		if len(pushResults) > 0 {
			benchmarkOutput.PushResults = append(benchmarkOutput.PushResults, aggregateResults(totalSize, pushResults))
		}
		if len(pullResults) > 0 {
			benchmarkOutput.PullResults = append(benchmarkOutput.PullResults, aggregateResults(totalSize, pullResults))
		}

		fmt.Fprintf(os.Stderr, "\n")
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

// pushBlobChunked pushes a blob using chunked uploads (PATCH requests)
func pushBlobChunked(ctx context.Context, registry, repository string, data []byte, dgst digest.Digest, chunkSize int) (BenchmarkResult, error) {
	client := &http.Client{
		Timeout: 30 * time.Minute,
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

	location := resp.Header.Get("Location")
	if location == "" {
		return BenchmarkResult{}, fmt.Errorf("no Location header in response")
	}

	if !strings.HasPrefix(location, "http") {
		location = registry + location
	}

	// Start timing the actual upload (chunks + final PUT)
	start := time.Now()

	// Step 2: Upload chunks (PATCH requests)
	totalSize := len(data)
	offset := 0

	for offset < totalSize {
		end := offset + chunkSize
		if end > totalSize {
			end = totalSize
		}
		chunk := data[offset:end]

		req, err = http.NewRequestWithContext(ctx, "PATCH", location, bytes.NewReader(chunk))
		if err != nil {
			return BenchmarkResult{}, fmt.Errorf("creating PATCH request: %w", err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Length", strconv.Itoa(len(chunk)))
		req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", offset, end-1))

		resp, err = client.Do(req)
		if err != nil {
			return BenchmarkResult{}, fmt.Errorf("uploading chunk at offset %d: %w", offset, err)
		}

		if resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return BenchmarkResult{}, fmt.Errorf("unexpected status %d for PATCH: %s", resp.StatusCode, string(body))
		}

		// Get updated location for next chunk
		newLocation := resp.Header.Get("Location")
		if newLocation != "" {
			if !strings.HasPrefix(newLocation, "http") {
				newLocation = registry + newLocation
			}
			location = newLocation
		}
		resp.Body.Close()

		offset = end
	}

	// Step 3: Complete upload (PUT with digest)
	if strings.Contains(location, "?") {
		location = location + "&digest=" + dgst.String()
	} else {
		location = location + "?digest=" + dgst.String()
	}

	req, err = http.NewRequestWithContext(ctx, "PUT", location, nil)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("creating PUT request: %w", err)
	}
	req.Header.Set("Content-Length", "0")

	resp, err = client.Do(req)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("completing upload: %w", err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return BenchmarkResult{}, fmt.Errorf("unexpected status %d for PUT: %s", resp.StatusCode, string(body))
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

// createManifest creates a Docker Schema v2 manifest referencing config and layer blobs
func createManifest(configDigest digest.Digest, configSize int64, layerDigests []digest.Digest, layerSizes []int64) benchmarkManifest {
	layers := make([]descriptorBenchmark, len(layerDigests))
	for i, dgst := range layerDigests {
		layers[i] = descriptorBenchmark{
			MediaType: mediaTypeLayer,
			Size:      layerSizes[i],
			Digest:    dgst.String(),
		}
	}

	return benchmarkManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeManifest,
		Config: descriptorBenchmark{
			MediaType: mediaTypeImageConfig,
			Size:      configSize,
			Digest:    configDigest.String(),
		},
		Layers: layers,
	}
}

// pushManifest pushes a manifest to the registry
func pushManifest(ctx context.Context, registry, repository, tag string, manifest benchmarkManifest) (time.Duration, error) {
	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return 0, fmt.Errorf("marshaling manifest: %w", err)
	}

	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", registry, repository, tag)

	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, "PUT", manifestURL, bytes.NewReader(manifestJSON))
	if err != nil {
		return 0, fmt.Errorf("creating PUT request: %w", err)
	}
	req.Header.Set("Content-Type", mediaTypeManifest)
	req.Header.Set("Content-Length", strconv.Itoa(len(manifestJSON)))

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("pushing manifest: %w", err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return duration, nil
}

// pullManifest pulls a manifest from the registry
func pullManifest(ctx context.Context, registry, repository, tag string) (benchmarkManifest, time.Duration, error) {
	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", registry, repository, tag)

	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, "GET", manifestURL, nil)
	if err != nil {
		return benchmarkManifest{}, 0, fmt.Errorf("creating GET request: %w", err)
	}
	req.Header.Set("Accept", mediaTypeManifest)

	resp, err := client.Do(req)
	if err != nil {
		return benchmarkManifest{}, 0, fmt.Errorf("fetching manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return benchmarkManifest{}, 0, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return benchmarkManifest{}, 0, fmt.Errorf("reading manifest body: %w", err)
	}

	duration := time.Since(start)

	var manifest benchmarkManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return benchmarkManifest{}, 0, fmt.Errorf("unmarshaling manifest: %w", err)
	}

	return manifest, duration, nil
}

// deleteManifest deletes a manifest from the registry
func deleteManifest(ctx context.Context, registry, repository, tag string) error {
	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", registry, repository, tag)

	req, err := http.NewRequestWithContext(ctx, "DELETE", manifestURL, nil)
	if err != nil {
		return fmt.Errorf("creating DELETE request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("deleting manifest: %w", err)
	}
	defer resp.Body.Close()

	// Accept 202 (Accepted) or 404 (Not Found - already deleted)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// benchmarkFullImagePush benchmarks a full image push (config + layers + manifest)
// Layers are pushed concurrently to simulate real Docker client behavior
func benchmarkFullImagePush(ctx context.Context, registry, repository, tag string, configData []byte, configDigest digest.Digest, layerData [][]byte, layerDigests []digest.Digest, useChunked bool, chunkSize int) (FullImageResult, error) {
	result := FullImageResult{
		NumLayers:     len(layerData),
		ChunkedUpload: useChunked,
		ChunkSize:     chunkSize,
	}

	totalStart := time.Now()

	// Push config blob
	var configResult BenchmarkResult
	var err error
	if useChunked {
		configResult, err = pushBlobChunked(ctx, registry, repository, configData, configDigest, chunkSize)
	} else {
		configResult, err = benchmarkPush(ctx, registry, repository, configData, configDigest, 0)
	}
	if err != nil {
		return result, fmt.Errorf("pushing config blob: %w", err)
	}
	result.ConfigPushDuration = configResult.Duration
	result.TotalBytes = int64(len(configData))

	// Push layer blobs concurrently (like Docker client does)
	result.LayerPushDurations = make([]int64, len(layerData))
	layerSizes := make([]int64, len(layerData))
	layerResults := make([]BenchmarkResult, len(layerData))
	layerErrors := make([]error, len(layerData))

	var wg sync.WaitGroup
	for i, data := range layerData {
		wg.Add(1)
		go func(idx int, layerBytes []byte, layerDigest digest.Digest) {
			defer wg.Done()
			var layerResult BenchmarkResult
			var layerErr error
			if useChunked {
				layerResult, layerErr = pushBlobChunked(ctx, registry, repository, layerBytes, layerDigest, chunkSize)
			} else {
				layerResult, layerErr = benchmarkPush(ctx, registry, repository, layerBytes, layerDigest, idx)
			}
			layerResults[idx] = layerResult
			layerErrors[idx] = layerErr
		}(i, data, layerDigests[i])
	}
	wg.Wait()

	// Check for errors and collect results
	for i := range layerData {
		if layerErrors[i] != nil {
			return result, fmt.Errorf("pushing layer %d: %w", i, layerErrors[i])
		}
		result.LayerPushDurations[i] = layerResults[i].Duration.Milliseconds()
		layerSizes[i] = int64(len(layerData[i]))
		result.TotalBytes += int64(len(layerData[i]))
	}

	// Create and push manifest
	manifest := createManifest(configDigest, int64(len(configData)), layerDigests, layerSizes)
	manifestDuration, err := pushManifest(ctx, registry, repository, tag, manifest)
	if err != nil {
		return result, fmt.Errorf("pushing manifest: %w", err)
	}
	result.ManifestPushDuration = manifestDuration

	result.TotalPushDuration = time.Since(totalStart)
	result.PushThroughputMBps = float64(result.TotalBytes) / result.TotalPushDuration.Seconds() / (1024 * 1024)

	return result, nil
}

// benchmarkFullImagePull benchmarks a full image pull (manifest + config + layers)
// Layers are pulled concurrently to simulate real Docker client behavior
func benchmarkFullImagePull(ctx context.Context, registry, repository, tag string, expectedTotalBytes int64) (FullImageResult, error) {
	result := FullImageResult{}

	totalStart := time.Now()

	// Pull manifest
	manifest, manifestDuration, err := pullManifest(ctx, registry, repository, tag)
	if err != nil {
		return result, fmt.Errorf("pulling manifest: %w", err)
	}
	result.ManifestPullDuration = manifestDuration
	result.NumLayers = len(manifest.Layers)

	// Pull config blob
	configDigest, err := digest.Parse(manifest.Config.Digest)
	if err != nil {
		return result, fmt.Errorf("parsing config digest: %w", err)
	}
	configResult, err := benchmarkPull(ctx, registry, repository, configDigest, manifest.Config.Size)
	if err != nil {
		return result, fmt.Errorf("pulling config blob: %w", err)
	}
	result.ConfigPullDuration = configResult.Duration
	result.TotalBytes = configResult.Size

	// Pull layer blobs concurrently (like Docker client does)
	result.LayerPullDurations = make([]int64, len(manifest.Layers))
	layerResults := make([]BenchmarkResult, len(manifest.Layers))
	layerErrors := make([]error, len(manifest.Layers))

	var wg sync.WaitGroup
	for i, layer := range manifest.Layers {
		wg.Add(1)
		go func(idx int, layerDesc descriptorBenchmark) {
			defer wg.Done()
			layerDigest, parseErr := digest.Parse(layerDesc.Digest)
			if parseErr != nil {
				layerErrors[idx] = fmt.Errorf("parsing layer %d digest: %w", idx, parseErr)
				return
			}
			layerResult, pullErr := benchmarkPull(ctx, registry, repository, layerDigest, layerDesc.Size)
			if pullErr != nil {
				layerErrors[idx] = pullErr
				return
			}
			layerResults[idx] = layerResult
		}(i, layer)
	}
	wg.Wait()

	// Check for errors and collect results
	for i := range manifest.Layers {
		if layerErrors[i] != nil {
			return result, fmt.Errorf("pulling layer %d: %w", i, layerErrors[i])
		}
		result.LayerPullDurations[i] = layerResults[i].Duration.Milliseconds()
		result.TotalBytes += layerResults[i].Size
	}

	result.TotalPullDuration = time.Since(totalStart)
	result.PullThroughputMBps = float64(result.TotalBytes) / result.TotalPullDuration.Seconds() / (1024 * 1024)

	return result, nil
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
		fmt.Println("+-----------+------------+-----------------+--------------+")
		fmt.Println("|   Size    | Iterations |   Throughput    |   Std Dev    |")
		fmt.Println("+-----------+------------+-----------------+--------------+")
		for _, r := range output.PushResults {
			fmt.Printf("| %9s | %10d | %10.2f MB/s | %8.2f MB/s |\n",
				r.SizeHuman, r.Iterations, r.MeanThroughput, r.StdDevThroughput)
		}
		fmt.Println("+-----------+------------+-----------------+--------------+")
		fmt.Println()
	}

	if len(output.PullResults) > 0 {
		fmt.Println("PULL RESULTS")
		fmt.Println("+-----------+------------+-----------------+--------------+")
		fmt.Println("|   Size    | Iterations |   Throughput    |   Std Dev    |")
		fmt.Println("+-----------+------------+-----------------+--------------+")
		for _, r := range output.PullResults {
			fmt.Printf("| %9s | %10d | %10.2f MB/s | %8.2f MB/s |\n",
				r.SizeHuman, r.Iterations, r.MeanThroughput, r.StdDevThroughput)
		}
		fmt.Println("+-----------+------------+-----------------+--------------+")
	}
}
