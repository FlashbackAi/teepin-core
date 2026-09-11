// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/models"
	"github.com/spf13/cobra"
)

// storageClient has no fixed timeout — unlike apiClient's 60s default,
// object uploads/downloads can legitimately take minutes for a large
// file and must not be cut off mid-transfer.
var storageClient = &http.Client{}

// storageDo issues an authenticated request against the object storage
// API with a raw (non-JSON) body — used for object uploads/downloads,
// where apiDo's JSON-body assumption doesn't apply. contentLength is
// only meaningful when body is non-nil: the server requires a declared
// Content-Length up front, which Go's http.Client only sends correctly
// when set on req.ContentLength, not as a header string.
func storageDo(method, rawURL string, body io.Reader, contentLength int64, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = contentLength
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if key, err := loadAPIKey(); err == nil && key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return storageClient.Do(req)
}

type storageBucketsListResponse struct {
	Buckets []models.Bucket `json:"buckets"`
}

type storageObjectsListResponse struct {
	Objects []models.StorageObject `json:"objects"`
}

type signedDownloadURLResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

var storageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Manage Teepin S3 buckets and objects",
	Long: `Create buckets, and upload, download, list, and delete objects in Teepin S3.

Examples:
  teepin storage buckets create my-bucket
  teepin storage objects put my-bucket photos/cat.jpg ./cat.jpg
  teepin storage objects get my-bucket photos/cat.jpg --outfile cat.jpg
`,
}

var storageBucketsCmd = &cobra.Command{
	Use:   "buckets",
	Short: "Manage buckets",
}

var storageObjectsCmd = &cobra.Command{
	Use:   "objects",
	Short: "Manage objects within a bucket",
}

var storageBucketsListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all buckets",
	Run:     runStorageBucketsList,
}

var storageBucketsCreateCmd = &cobra.Command{
	Use:   "create NAME",
	Short: "Create a new bucket",
	Args:  cobra.ExactArgs(1),
	Run:   runStorageBucketsCreate,
}

var storageBucketsDeleteCmd = &cobra.Command{
	Use:     "delete NAME",
	Aliases: []string{"rm"},
	Short:   "Delete a bucket (must be empty)",
	Args:    cobra.ExactArgs(1),
	Run:     runStorageBucketsDelete,
}

var storageObjectsListCmd = &cobra.Command{
	Use:     "list BUCKET",
	Aliases: []string{"ls"},
	Short:   "List objects in a bucket",
	Long: `List objects in a bucket, optionally filtered by prefix.

Examples:
  teepin storage objects list my-bucket
  teepin storage objects list my-bucket --prefix photos/
`,
	Args: cobra.ExactArgs(1),
	Run:  runStorageObjectsList,
}

var storageObjectsPutCmd = &cobra.Command{
	Use:   "put BUCKET KEY FILE",
	Short: "Upload a local file to a bucket",
	Long: `Upload a local file's content to the given bucket and key.

The call does not return until the write has actually succeeded or
failed against the real storage backend — there is no staging step, so
a large file legitimately takes as long as it takes to upload.

Examples:
  teepin storage objects put my-bucket photos/cat.jpg ./cat.jpg
  teepin storage objects put my-bucket data/report.csv ./report.csv --content-type text/csv --metadata source=nightly-job
`,
	Args: cobra.ExactArgs(3),
	Run:  runStorageObjectsPut,
}

var storageObjectsGetCmd = &cobra.Command{
	Use:   "get BUCKET KEY",
	Short: "Download an object's content",
	Long: `Download an object's content, either to a file or to stdout.

Examples:
  teepin storage objects get my-bucket photos/cat.jpg --outfile cat.jpg
  teepin storage objects get my-bucket photos/cat.jpg > cat.jpg
`,
	Args: cobra.ExactArgs(2),
	Run:  runStorageObjectsGet,
}

var storageObjectsDeleteCmd = &cobra.Command{
	Use:     "delete BUCKET KEY",
	Aliases: []string{"rm"},
	Short:   "Delete an object",
	Args:    cobra.ExactArgs(2),
	Run:     runStorageObjectsDelete,
}

var storageObjectsURLCmd = &cobra.Command{
	Use:   "url BUCKET KEY",
	Short: "Mint a short-lived signed download link",
	Long: `Mint a short-lived, pre-authorized link to an object.

The returned URL carries no API key and is safe to hand directly to an
end user or embed in a browser — this is the supported way to let a
customer's own users fetch a Teepin S3 object without ever seeing the
API key. There is no equivalent for uploads: a client with no backend
of its own cannot safely upload to Teepin S3 today.

Examples:
  teepin storage objects url my-bucket photos/cat.jpg
  teepin storage objects url my-bucket photos/cat.jpg --ttl 300 --disposition attachment
`,
	Args: cobra.ExactArgs(2),
	Run:  runStorageObjectsURL,
}

func init() {
	rootCmd.AddCommand(storageCmd)
	storageCmd.AddCommand(storageBucketsCmd)
	storageCmd.AddCommand(storageObjectsCmd)

	storageBucketsCmd.AddCommand(storageBucketsListCmd)
	storageBucketsCmd.AddCommand(storageBucketsCreateCmd)
	storageBucketsCmd.AddCommand(storageBucketsDeleteCmd)

	storageObjectsCmd.AddCommand(storageObjectsListCmd)
	storageObjectsCmd.AddCommand(storageObjectsPutCmd)
	storageObjectsCmd.AddCommand(storageObjectsGetCmd)
	storageObjectsCmd.AddCommand(storageObjectsDeleteCmd)
	storageObjectsCmd.AddCommand(storageObjectsURLCmd)

	storageObjectsListCmd.Flags().String("prefix", "", "Only list keys with this prefix")
	storageObjectsListCmd.Flags().String("cursor", "", "Pagination cursor from a previous list")
	storageObjectsListCmd.Flags().Int("limit", 0, "Maximum number of keys to return")

	storageObjectsPutCmd.Flags().String("content-type", "", "Content-Type to store with the object (guessed from the file extension if omitted)")
	storageObjectsPutCmd.Flags().StringToString("metadata", nil, "Custom metadata as key=value pairs (repeatable)")

	storageObjectsGetCmd.Flags().String("outfile", "", "Write to this file instead of stdout")
	storageObjectsGetCmd.Flags().String("range", "", "Byte range to fetch, e.g. bytes=0-1023")

	storageObjectsURLCmd.Flags().Int("ttl", 0, "Link lifetime in seconds (default: server default, 15 minutes)")
	storageObjectsURLCmd.Flags().String("disposition", "inline", `"inline" (preview) or "attachment" (force download)`)
}

func runStorageBucketsList(cmd *cobra.Command, args []string) {

	resp, err := apiDo(http.MethodGet, getAPIURL()+"/v1/storage/buckets", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to API: %v\n", err)
		fmt.Fprintf(os.Stderr, "   Make sure the API server is running at: %s\n", getAPIURL())
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Failed to list buckets\n   Status: %d\n   Response: %s%s\n", resp.StatusCode, string(body), authHint(resp.StatusCode))
		os.Exit(1)
	}

	if output == "json" {
		fmt.Println(string(body))
		return
	}

	var listResp storageBucketsListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		os.Exit(1)
	}

	if len(listResp.Buckets) == 0 {
		fmt.Println("No buckets found.")
		fmt.Println()
		fmt.Println("To create one:")
		fmt.Println("  teepin storage buckets create my-bucket")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tBACKEND\tOBJECTS\tSIZE\tCREATED")
	fmt.Fprintln(w, "----\t-------\t-------\t----\t-------")
	for _, b := range listResp.Buckets {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
			b.Name, b.Backend, b.ObjectCount, formatBytesCLI(b.TotalBytes), b.CreatedAt.Format("2006-01-02 15:04"))
	}
	w.Flush()
}

func runStorageBucketsCreate(cmd *cobra.Command, args []string) {
	name := args[0]

	reqBody, _ := json.Marshal(map[string]string{"name": name})
	resp, err := apiDo(http.MethodPost, getAPIURL()+"/v1/storage/buckets", bytes.NewBuffer(reqBody))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to API: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		fmt.Fprintf(os.Stderr, "Failed to create bucket\n   Status: %d\n   Response: %s%s\n", resp.StatusCode, string(body), authHint(resp.StatusCode))
		os.Exit(1)
	}

	if output == "json" {
		fmt.Println(string(body))
		return
	}

	var bucket models.Bucket
	if err := json.Unmarshal(body, &bucket); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Bucket created: %s (backend: %s)\n", bucket.Name, bucket.Backend)
}

func runStorageBucketsDelete(cmd *cobra.Command, args []string) {
	name := args[0]

	resp, err := apiDo(http.MethodDelete, getAPIURL()+"/v1/storage/buckets/"+url.PathEscape(name), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to API: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Failed to delete bucket\n   Status: %d\n   Response: %s%s\n", resp.StatusCode, string(body), authHint(resp.StatusCode))
		os.Exit(1)
	}
	fmt.Printf("Deleted bucket: %s\n", name)
}

func runStorageObjectsList(cmd *cobra.Command, args []string) {
	bucket := args[0]

	prefix, _ := cmd.Flags().GetString("prefix")
	cursor, _ := cmd.Flags().GetString("cursor")
	limit, _ := cmd.Flags().GetInt("limit")

	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	apiURL := getAPIURL() + "/v1/storage/buckets/" + url.PathEscape(bucket) + "/objects"
	if enc := q.Encode(); enc != "" {
		apiURL += "?" + enc
	}

	resp, err := apiDo(http.MethodGet, apiURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to API: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Failed to list objects\n   Status: %d\n   Response: %s%s\n", resp.StatusCode, string(body), authHint(resp.StatusCode))
		os.Exit(1)
	}

	if output == "json" {
		fmt.Println(string(body))
		return
	}

	var listResp storageObjectsListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		os.Exit(1)
	}

	if len(listResp.Objects) == 0 {
		fmt.Println("No objects found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tSIZE\tSTATUS\tCONTENT TYPE\tUPDATED")
	fmt.Fprintln(w, "---\t----\t------\t------------\t-------")
	for _, o := range listResp.Objects {
		ct := o.ContentType
		if ct == "" {
			ct = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			o.Key, formatBytesCLI(o.SizeBytes), o.Status, ct, o.UpdatedAt.Format("2006-01-02 15:04"))
	}
	w.Flush()
}

func runStorageObjectsPut(cmd *cobra.Command, args []string) {
	bucket, key, localPath := args[0], args[1], args[2]

	f, err := os.Open(localPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file info: %v\n", err)
		os.Exit(1)
	}

	contentType, _ := cmd.Flags().GetString("content-type")
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(localPath))
	}
	metadata, _ := cmd.Flags().GetStringToString("metadata")

	headers := map[string]string{}
	if contentType != "" {
		headers["Content-Type"] = contentType
	}
	for k, v := range metadata {
		headers["X-Teepin-Meta-"+k] = v
	}

	apiURL := getAPIURL() + "/v1/storage/buckets/" + url.PathEscape(bucket) + "/object?" + url.Values{"key": {key}}.Encode()
	resp, err := storageDo(http.MethodPut, apiURL, f, info.Size(), headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to API: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Failed to upload object\n   Status: %d\n   Response: %s%s\n", resp.StatusCode, string(body), authHint(resp.StatusCode))
		os.Exit(1)
	}

	if output == "json" {
		fmt.Println(string(body))
		return
	}

	var obj models.StorageObject
	if err := json.Unmarshal(body, &obj); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Uploaded: %s (%s, status: %s)\n", obj.Key, formatBytesCLI(obj.SizeBytes), obj.Status)
}

func runStorageObjectsGet(cmd *cobra.Command, args []string) {
	bucket, key := args[0], args[1]

	outfile, _ := cmd.Flags().GetString("outfile")
	rangeHeader, _ := cmd.Flags().GetString("range")

	headers := map[string]string{}
	if rangeHeader != "" {
		headers["Range"] = rangeHeader
	}

	apiURL := getAPIURL() + "/v1/storage/buckets/" + url.PathEscape(bucket) + "/object/content?" + url.Values{"key": {key}}.Encode()
	resp, err := storageDo(http.MethodGet, apiURL, nil, 0, headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to API: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Failed to download object\n   Status: %d\n   Response: %s%s\n", resp.StatusCode, string(body), authHint(resp.StatusCode))
		os.Exit(1)
	}

	// When streaming to stdout, print nothing else — the output may be
	// piped straight into a file or another tool.
	var out io.Writer = os.Stdout
	if outfile != "" {
		f, err := os.Create(outfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating output file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}

	n, err := io.Copy(out, resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error downloading object: %v\n", err)
		os.Exit(1)
	}

	if outfile != "" {
		fmt.Printf("Downloaded %s to %s\n", formatBytesCLI(n), outfile)
	}
}

func runStorageObjectsDelete(cmd *cobra.Command, args []string) {
	bucket, key := args[0], args[1]

	apiURL := getAPIURL() + "/v1/storage/buckets/" + url.PathEscape(bucket) + "/object?" + url.Values{"key": {key}}.Encode()
	resp, err := apiDo(http.MethodDelete, apiURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to API: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Failed to delete object\n   Status: %d\n   Response: %s%s\n", resp.StatusCode, string(body), authHint(resp.StatusCode))
		os.Exit(1)
	}
	fmt.Printf("Deleted object: %s/%s\n", bucket, key)
}

func runStorageObjectsURL(cmd *cobra.Command, args []string) {
	bucket, key := args[0], args[1]

	ttl, _ := cmd.Flags().GetInt("ttl")
	disposition, _ := cmd.Flags().GetString("disposition")

	q := url.Values{"key": {key}}
	if ttl > 0 {
		q.Set("ttl_seconds", strconv.Itoa(ttl))
	}
	if disposition != "" {
		q.Set("disposition", disposition)
	}

	apiURL := getAPIURL() + "/v1/storage/buckets/" + url.PathEscape(bucket) + "/object/download-url?" + q.Encode()
	resp, err := apiDo(http.MethodPost, apiURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to API: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Failed to mint download link\n   Status: %d\n   Response: %s%s\n", resp.StatusCode, string(body), authHint(resp.StatusCode))
		os.Exit(1)
	}

	if output == "json" {
		fmt.Println(string(body))
		return
	}

	var link signedDownloadURLResponse
	if err := json.Unmarshal(body, &link); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s%s\n", getAPIURL(), link.URL)
	fmt.Printf("Expires: %s\n", link.ExpiresAt.Format(time.RFC3339))
}

// formatBytesCLI renders a byte count the way the console's formatBytes
// helper does, so `teepin storage` output matches what the web console
// shows for the same bucket/object.
func formatBytesCLI(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
