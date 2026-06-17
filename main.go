package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

//go:embed web/*
var webAssets embed.FS

var (
	s3Client   *s3.Client
	bucketName = "serv-packages"
)

type PackageInfo struct {
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

func main() {
	addr := flag.String("addr", ":8088", "Registry server listen address")
	s3Endpoint := flag.String("s3-endpoint", "http://localhost:9000", "ServStore/S3 endpoint URL")
	s3AccessKey := flag.String("s3-access-key", "admin", "S3 access key")
	s3SecretKey := flag.String("s3-secret-key", "admin123", "S3 secret key")
	flag.Parse()

	// Override with env variables if set
	if envPort := os.Getenv("PORT"); envPort != "" {
		*addr = ":" + envPort
	}
	if envEndpoint := os.Getenv("SERV_STORE_ENDPOINT"); envEndpoint != "" {
		*s3Endpoint = envEndpoint
	}
	if envAccessKey := os.Getenv("SERV_STORE_ACCESS_KEY"); envAccessKey != "" {
		*s3AccessKey = envAccessKey
	}
	if envSecretKey := os.Getenv("SERV_STORE_SECRET_KEY"); envSecretKey != "" {
		*s3SecretKey = envSecretKey
	}

	log.Printf("Connecting to ServStore S3 at %s...", *s3Endpoint)

	// Configure S3 Client
	customResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			URL:               *s3Endpoint,
			SigningRegion:     "us-east-1",
			HostnameImmutable: true,
		}, nil
	})

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithEndpointResolverWithOptions(customResolver),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(*s3AccessKey, *s3SecretKey, "")),
	)
	if err != nil {
		log.Fatalf("Unable to load S3 SDK config: %v", err)
	}

	s3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	// Ensure bucket exists
	ensureBucketExists(context.Background())

	// Set up router
	mux := http.NewServeMux()

	// Publish API
	mux.HandleFunc("/publish", handlePublish)

	// Install/Fetch package tarball API
	mux.HandleFunc("/packages/", handleGetPackage)

	// API to list packages
	mux.HandleFunc("/api/packages", handleListPackages)

	// Web dashboard static files
	mux.HandleFunc("/", handleWebDashboard)

	log.Printf("ServRegistry running on http://localhost%s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}

func ensureBucketExists(ctx context.Context) {
	_, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err == nil {
		log.Printf("Bucket '%s' verified", bucketName)
		return
	}

	log.Printf("Bucket '%s' does not exist. Creating it...", bucketName)
	_, err = s3Client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		log.Fatalf("Failed to create bucket '%s': %v", bucketName, err)
	}
	log.Printf("Bucket '%s' successfully created", bucketName)
}

func handlePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pkgName := r.URL.Query().Get("name")
	if pkgName == "" {
		http.Error(w, "Missing 'name' query parameter", http.StatusBadRequest)
		return
	}

	// Sanitize name
	pkgName = strings.TrimSpace(filepath.Base(pkgName))
	if pkgName == "" || pkgName == "." || pkgName == "/" {
		http.Error(w, "Invalid package name", http.StatusBadRequest)
		return
	}

	log.Printf("Publishing package: %s", pkgName)

	// Read body in memory to make it a seekable reader (fixes S3 PutObject stream error)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Failed to read body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Upload to ServStore
	objectKey := fmt.Sprintf("%s.tar.gz", pkgName)
	_, err = s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(bucketName),
		Key:         aws.String(objectKey),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		log.Printf("Failed to upload to S3: %v", err)
		http.Error(w, "Failed to upload package to storage: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "✓ Package '%s' successfully published to registry!\n", pkgName)
}

func handleGetPackage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Path will be "/packages/{name}.tar.gz"
	path := strings.TrimPrefix(r.URL.Path, "/packages/")
	if path == "" {
		http.Error(w, "Missing package filename", http.StatusBadRequest)
		return
	}

	log.Printf("Fetching package: %s", path)

	resp, err := s3Client.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(path),
	})
	if err != nil {
		log.Printf("Failed to get object from S3: %v", err)
		http.Error(w, "Package not found", http.StatusNotFound)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("Error copying package body: %v", err)
	}
}

func handleListPackages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp, err := s3Client.ListObjectsV2(r.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		log.Printf("Failed to list objects in S3: %v", err)
		http.Error(w, "Failed to list packages", http.StatusInternalServerError)
		return
	}

	packages := []PackageInfo{}
	for _, obj := range resp.Contents {
		name := *obj.Key
		if strings.HasSuffix(name, ".tar.gz") {
			name = strings.TrimSuffix(name, ".tar.gz")
		}

		var lastModified time.Time
		if obj.LastModified != nil {
			lastModified = *obj.LastModified
		}

		var size int64
		if obj.Size != nil {
			size = *obj.Size
		}

		packages = append(packages, PackageInfo{
			Name:         name,
			Size:         size,
			LastModified: lastModified,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(packages)
}

func handleWebDashboard(w http.ResponseWriter, r *http.Request) {
	// Serve embedded dashboard static files
	path := r.URL.Path
	if path == "/" {
		path = "/web/index.html"
	} else {
		path = "/web" + path
	}

	// Try reading file from embedded fs
	data, err := webAssets.ReadFile(strings.TrimPrefix(path, "/"))
	if err != nil {
		// Fallback to index.html for single page app routing or 404
		data, err = webAssets.ReadFile("web/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		path = "/web/index.html"
	}

	// Set content type
	switch {
	case strings.HasSuffix(path, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	}

	w.Write(data)
}
