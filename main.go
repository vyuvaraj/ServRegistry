package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

	packageIndexMu sync.RWMutex
	packageIndex   = make(map[string]*PackageIndexItem)
)

type PackageIndexItem struct {
	Name         string    `json:"name"`
	Latest       string    `json:"latest"`
	Versions     []string  `json:"versions"`
	LastModified time.Time `json:"lastModified"`
}

type PackageMetadata struct {
	Name     string                    `json:"name"`
	Versions map[string]VersionDetails `json:"versions"`
}

type VersionDetails struct {
	Version      string   `json:"version"`
	Dependencies []string `json:"dependencies"`
	Size         int64    `json:"size"`
	PublishedAt  string   `json:"publishedAt"`
}

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

	// Build package index on startup
	go buildPackageIndex(context.Background())

	// Set up router
	mux := http.NewServeMux()

	// Health probes
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"healthy"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"healthy"}`))
	})

	// Publish API
	mux.HandleFunc("/publish", handlePublish)
	mux.HandleFunc("/api/v1/publish", handlePublish)

	// Install/Fetch package tarball API
	mux.HandleFunc("/packages/", handleGetPackage)
	mux.HandleFunc("/api/v1/packages/", handleGetPackage)

	// Search API
	mux.HandleFunc("/api/packages/search", handleSearchPackages)
	mux.HandleFunc("/api/v1/packages/search", handleSearchPackages)

	// API to list packages or versions
	mux.HandleFunc("/api/packages/", handlePackagesAPI)
	mux.HandleFunc("/api/v1/packages/", handlePackagesAPI)

	// Web dashboard static files
	mux.HandleFunc("/", handleWebDashboard)

	log.Printf("ServRegistry running on http://localhost%s", *addr)
	server := &http.Server{
		Addr:    *addr,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Registry: Shutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Registry: Server forced to shutdown: %v", err)
	} else {
		log.Println("Registry: Server exited cleanly")
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
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	// 1. Enforce JWT validation if SERV_JWT_SECRET is set
	if jwtSecret := os.Getenv("SERV_JWT_SECRET"); jwtSecret != "" {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			WriteJSONError(w, r, "Unauthorized: Missing or invalid token", "ERR_UNAUTHORIZED", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if _, ok := validateJWT(token, []byte(jwtSecret)); !ok {
			WriteJSONError(w, r, "Unauthorized: Invalid JWT", "ERR_INVALID_JWT", http.StatusUnauthorized)
			return
		}
	}

	// Read body in memory
	data, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Failed to read body: %v", err)
		WriteJSONError(w, r, "Failed to read request body", "ERR_BAD_REQUEST_BODY", http.StatusBadRequest)
		return
	}

	// 2. Parse serv.toml from uploaded tar.gz
	name, version, deps, err := parseServTomlFromTarGz(data)
	if err != nil {
		// Fallback to query parameter name & default version
		log.Printf("Manifest parsing failed or not found: %v. Using fallback", err)
		name = r.URL.Query().Get("name")
		if name == "" {
			WriteJSONError(w, r, "Missing 'name' parameter and no serv.toml found", "ERR_MISSING_NAME_PARAMETER", http.StatusBadRequest)
			return
		}
		version = r.URL.Query().Get("version")
		if version == "" {
			version = "0.0.0"
		}
		deps = []string{}
	}

	// Sanitize name
	name = strings.TrimSpace(filepath.Base(name))
	version = strings.TrimSpace(filepath.Base(version))
	if name == "" || name == "." || name == "/" || version == "" {
		WriteJSONError(w, r, "Invalid package name or version", "ERR_INVALID_PACKAGE_VERSION", http.StatusBadRequest)
		return
	}

	log.Printf("Publishing package: %s @ %s", name, version)

	// 3. Update metadata.json on S3
	metadataKey := fmt.Sprintf("%s/metadata.json", name)
	var metadata PackageMetadata
	metadata.Name = name
	metadata.Versions = make(map[string]VersionDetails)

	// Try to load existing metadata.json
	metaResp, err := s3Client.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(metadataKey),
	})
	if err == nil {
		metaData, merr := io.ReadAll(metaResp.Body)
		metaResp.Body.Close()
		if merr == nil {
			_ = json.Unmarshal(metaData, &metadata)
		}
	}

	// Add/update version details
	metadata.Versions[version] = VersionDetails{
		Version:      version,
		Dependencies: deps,
		Size:         int64(len(data)),
		PublishedAt:  time.Now().Format(time.RFC3339),
	}

	updatedMetaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		WriteJSONError(w, r, "Failed to serialize metadata", "ERR_INTERNAL_SERVER_ERROR", http.StatusInternalServerError)
		return
	}

	// Upload updated metadata.json
	_, err = s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(bucketName),
		Key:         aws.String(metadataKey),
		Body:        bytes.NewReader(updatedMetaBytes),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		log.Printf("Failed to upload metadata to S3: %v", err)
		WriteJSONError(w, r, "Failed to upload metadata", "ERR_METADATA_UPLOAD_FAILED", http.StatusInternalServerError)
		return
	}

	// 4. Upload tarball payload to S3 key structure: {name}/{version}/{name}-{version}.tar.gz
	objectKey := fmt.Sprintf("%s/%s/%s-%s.tar.gz", name, version, name, version)
	_, err = s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(bucketName),
		Key:         aws.String(objectKey),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		log.Printf("Failed to upload package tarball to S3: %v", err)
		WriteJSONError(w, r, "Failed to upload package to storage: "+err.Error(), "ERR_PACKAGE_UPLOAD_FAILED", http.StatusInternalServerError)
		return
	}

	// Proactively update local cache index
	packageIndexMu.Lock()
	versions := []string{}
	var latest string
	var latestTime time.Time
	for v, details := range metadata.Versions {
		versions = append(versions, v)
		t, err := time.Parse(time.RFC3339, details.PublishedAt)
		if err == nil && t.After(latestTime) {
			latestTime = t
			latest = v
		}
	}
	packageIndex[name] = &PackageIndexItem{
		Name:         name,
		Latest:       latest,
		Versions:     versions,
		LastModified: latestTime,
	}
	packageIndexMu.Unlock()

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "✓ Package '%s' @ '%s' successfully published to registry!\n", name, version)
}

func handleGetPackage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	// Path will be "/packages/{name}.tar.gz" or "/packages/{name}/{version}/{name}-{version}.tar.gz"
	var path string
	if strings.HasPrefix(r.URL.Path, "/api/v1/packages/") {
		path = strings.TrimPrefix(r.URL.Path, "/api/v1/packages/")
	} else {
		path = strings.TrimPrefix(r.URL.Path, "/packages/")
	}
	if path == "" {
		WriteJSONError(w, r, "Missing package filename", "ERR_MISSING_FILENAME", http.StatusBadRequest)
		return
	}

	log.Printf("Fetching package: %s", path)

	var s3Key string
	// Check if path is simple name (e.g. "mypkg.tar.gz") vs full version key (e.g. "mypkg/1.0.0/mypkg-1.0.0.tar.gz")
	if !strings.Contains(path, "/") && strings.HasSuffix(path, ".tar.gz") {
		name := strings.TrimSuffix(path, ".tar.gz")
		// Fetch metadata to find the latest version
		metadataKey := fmt.Sprintf("%s/metadata.json", name)
		metaResp, err := s3Client.GetObject(r.Context(), &s3.GetObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(metadataKey),
		})
		if err != nil {
			// Backwards compatibility: maybe it was stored as mypkg.tar.gz in the S3 root?
			s3Key = path
		} else {
			defer metaResp.Body.Close()
			var metadata PackageMetadata
			metaData, merr := io.ReadAll(metaResp.Body)
			if merr == nil && json.Unmarshal(metaData, &metadata) == nil && len(metadata.Versions) > 0 {
				// Find latest version
				var latest string
				var latestTime time.Time
				for v, details := range metadata.Versions {
					t, err := time.Parse(time.RFC3339, details.PublishedAt)
					if err == nil && t.After(latestTime) {
						latestTime = t
						latest = v
					} else if latest == "" {
						latest = v
					}
				}
				s3Key = fmt.Sprintf("%s/%s/%s-%s.tar.gz", name, latest, name, latest)
			} else {
				s3Key = path
			}
		}
	} else {
		s3Key = path
	}

	resp, err := s3Client.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		log.Printf("Failed to get object from S3: %v", err)
		WriteJSONError(w, r, "Package not found", "ERR_PACKAGE_NOT_FOUND", http.StatusNotFound)
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
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	packageIndexMu.RLock()
	packages := make([]PackageIndexItem, 0, len(packageIndex))
	for _, item := range packageIndex {
		packages = append(packages, *item)
	}
	packageIndexMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(packages)
}

func handlePackagesAPI(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/versions") {
		handleGetVersions(w, r)
		return
	}
	if strings.Contains(r.URL.Path, "/deps") {
		handleGetDeps(w, r)
		return
	}
	handleListPackages(w, r)
}

func handleGetDeps(w http.ResponseWriter, r *http.Request) {
	// Path: /api/packages/{name}/deps or /api/packages/{name}/{version}/deps
	var path string
	if strings.HasPrefix(r.URL.Path, "/api/v1/packages/") {
		path = strings.TrimPrefix(r.URL.Path, "/api/v1/packages/")
	} else {
		path = strings.TrimPrefix(r.URL.Path, "/api/packages/")
	}
	path = strings.TrimSuffix(path, "/deps")
	parts := strings.Split(path, "/")

	var name, version string
	if len(parts) == 1 {
		name = parts[0]
	} else if len(parts) == 2 {
		name = parts[0]
		version = parts[1]
	} else {
		WriteJSONError(w, r, "Invalid path", "ERR_INVALID_PATH", http.StatusBadRequest)
		return
	}

	// Fetch metadata
	metadataKey := fmt.Sprintf("%s/metadata.json", name)
	resp, err := s3Client.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(metadataKey),
	})
	if err != nil {
		WriteJSONError(w, r, "Package not found", "ERR_PACKAGE_NOT_FOUND", http.StatusNotFound)
		return
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		WriteJSONError(w, r, "Failed to read metadata", "ERR_INTERNAL_SERVER_ERROR", http.StatusInternalServerError)
		return
	}

	var metadata PackageMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		WriteJSONError(w, r, "Failed to parse metadata", "ERR_INTERNAL_SERVER_ERROR", http.StatusInternalServerError)
		return
	}

	// If no version specified, use latest
	if version == "" {
		var latestTime time.Time
		for v, details := range metadata.Versions {
			t, err := time.Parse(time.RFC3339, details.PublishedAt)
			if err == nil && t.After(latestTime) {
				latestTime = t
				version = v
			}
		}
	}

	versionDetails, ok := metadata.Versions[version]
	if !ok {
		WriteJSONError(w, r, fmt.Sprintf("Version %s not found", version), "ERR_VERSION_NOT_FOUND", http.StatusNotFound)
		return
	}

	// Resolve full dependency tree (BFS)
	type DepNode struct {
		Name         string   `json:"name"`
		Version      string   `json:"version"`
		Dependencies []string `json:"dependencies"`
	}

	resolved := []DepNode{{
		Name:         name,
		Version:      version,
		Dependencies: versionDetails.Dependencies,
	}}

	seen := map[string]bool{name: true}
	queue := versionDetails.Dependencies

	for len(queue) > 0 {
		dep := queue[0]
		queue = queue[1:]

		// Parse "pkgname@version"
		depParts := strings.SplitN(dep, "@", 2)
		depName := depParts[0]
		if seen[depName] {
			continue
		}
		seen[depName] = true

		depMetaKey := fmt.Sprintf("%s/metadata.json", depName)
		depResp, err := s3Client.GetObject(r.Context(), &s3.GetObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(depMetaKey),
		})
		if err != nil {
			// Dependency not found in registry — skip (might be stdlib)
			resolved = append(resolved, DepNode{Name: depName, Version: "unknown", Dependencies: nil})
			continue
		}

		depData, _ := io.ReadAll(depResp.Body)
		depResp.Body.Close()

		var depMeta PackageMetadata
		if err := json.Unmarshal(depData, &depMeta); err != nil {
			continue
		}

		// Resolve version: use requested version or latest
		depVersion := ""
		if len(depParts) == 2 {
			depVersion = depParts[1]
		}
		if depVersion == "" || depMeta.Versions[depVersion].Version == "" {
			var latestTime time.Time
			for v, details := range depMeta.Versions {
				t, err := time.Parse(time.RFC3339, details.PublishedAt)
				if err == nil && t.After(latestTime) {
					latestTime = t
					depVersion = v
				}
			}
		}

		if vd, ok := depMeta.Versions[depVersion]; ok {
			resolved = append(resolved, DepNode{Name: depName, Version: depVersion, Dependencies: vd.Dependencies})
			queue = append(queue, vd.Dependencies...)
		} else {
			resolved = append(resolved, DepNode{Name: depName, Version: depVersion, Dependencies: nil})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"package":  name,
		"version":  version,
		"tree":     resolved,
		"resolved": len(resolved),
	})
}

func handleGetVersions(w http.ResponseWriter, r *http.Request) {
	// Path will be /api/packages/{name}/versions or /api/v1/packages/{name}/versions
	var path string
	if strings.HasPrefix(r.URL.Path, "/api/v1/packages/") {
		path = strings.TrimPrefix(r.URL.Path, "/api/v1/packages/")
	} else {
		path = strings.TrimPrefix(r.URL.Path, "/api/packages/")
	}
	path = strings.TrimSuffix(path, "/versions")
	name := strings.TrimSpace(path)
	if name == "" {
		WriteJSONError(w, r, "Missing package name", "ERR_MISSING_NAME_PARAMETER", http.StatusBadRequest)
		return
	}

	metadataKey := fmt.Sprintf("%s/metadata.json", name)
	resp, err := s3Client.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(metadataKey),
	})
	if err != nil {
		WriteJSONError(w, r, "Package not found", "ERR_PACKAGE_NOT_FOUND", http.StatusNotFound)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}

func handleSearchPackages(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(r.URL.Query().Get("q"))

	packageIndexMu.RLock()
	results := []*PackageIndexItem{}
	for name, item := range packageIndex {
		if query == "" || strings.Contains(strings.ToLower(name), query) {
			results = append(results, item)
		}
	}
	packageIndexMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func buildPackageIndex(ctx context.Context) {
	resp, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		log.Printf("Failed to list objects for indexing: %v", err)
		return
	}

	packageIndexMu.Lock()
	defer packageIndexMu.Unlock()

	packageIndex = make(map[string]*PackageIndexItem)

	for _, obj := range resp.Contents {
		key := *obj.Key
		if strings.HasSuffix(key, "/metadata.json") {
			mResp, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(bucketName),
				Key:    aws.String(key),
			})
			if err != nil {
				continue
			}
			data, err := io.ReadAll(mResp.Body)
			mResp.Body.Close()
			if err != nil {
				continue
			}

			var meta PackageMetadata
			if err := json.Unmarshal(data, &meta); err == nil {
				versions := []string{}
				var latest string
				var latestTime time.Time
				for v, details := range meta.Versions {
					versions = append(versions, v)
					t, err := time.Parse(time.RFC3339, details.PublishedAt)
					if err == nil && t.After(latestTime) {
						latestTime = t
						latest = v
					} else if latest == "" {
						latest = v
					}
				}
				packageIndex[meta.Name] = &PackageIndexItem{
					Name:         meta.Name,
					Latest:       latest,
					Versions:     versions,
					LastModified: latestTime,
				}
			}
		}
	}
	log.Printf("Package index built: %d packages found", len(packageIndex))
}

func parseServTomlFromTarGz(data []byte) (string, string, []string, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", "", nil, err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", nil, err
		}

		if filepath.Base(hdr.Name) == "serv.toml" {
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, tr); err != nil {
				return "", "", nil, err
			}
			return parseServToml(buf.String())
		}
	}
	return "", "", nil, fmt.Errorf("serv.toml not found in package archive")
}

func parseServToml(content string) (string, string, []string, error) {
	var name, version string
	var dependencies []string

	lines := strings.Split(content, "\n")
	inDependenciesSection := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(line[1 : len(line)-1])
			if section == "dependencies" {
				inDependenciesSection = true
			} else {
				inDependenciesSection = false
			}
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])
		if (strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"")) ||
			(strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'")) {
			v = v[1 : len(v)-1]
		}

		if inDependenciesSection {
			dependencies = append(dependencies, fmt.Sprintf("%s@%s", k, v))
		} else {
			if k == "name" {
				name = v
			} else if k == "version" {
				version = v
			}
		}
	}
	return name, version, dependencies, nil
}

func validateJWT(tokenStr string, secret []byte) (string, bool) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return "", false
	}

	headerPart, payloadPart, signaturePart := parts[0], parts[1], parts[2]

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(headerPart + "." + payloadPart))
	expectedMac := mac.Sum(nil)

	sigBytes, err := base64UrlDecode(signaturePart)
	if err != nil || !hmac.Equal(sigBytes, expectedMac) {
		return "", false
	}

	payloadBytes, err := base64UrlDecode(payloadPart)
	if err != nil {
		return "", false
	}

	var claims struct {
		Username string `json:"username"`
		Exp      int64  `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return "", false
	}

	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return "", false
	}

	return claims.Username, true
}

func base64UrlDecode(s string) ([]byte, error) {
	s = strings.TrimSuffix(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
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

type APIError struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	TraceID string `json:"trace_id,omitempty"`
}

func WriteJSONError(w http.ResponseWriter, r *http.Request, msg string, code string, status int) {
	traceID := ""
	if r != nil {
		traceparent := r.Header.Get("traceparent")
		if traceparent != "" {
			parts := strings.Split(traceparent, "-")
			if len(parts) >= 2 {
				traceID = parts[1]
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIError{
		Error:   msg,
		Code:    code,
		TraceID: traceID,
	})
}
