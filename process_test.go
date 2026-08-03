package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// BenchmarkProcessImage benchmarks processImage directly using an in-memory DB
// and overwrites thumbnails in ./tests/testdata/test_thumbnails/cache/small
func BenchmarkProcessImage(b *testing.B) {
	// 1. Define input and output directories
	inputDir := "./tests/testdata/sample_photos"
	testOutputDir := "./tests/testdata/test_thumbnails"
	smallThumbDir := filepath.Join(testOutputDir, "cache", "small")

	// Read image file paths
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		b.Fatalf("Failed to read test image directory (%s): %v", inputDir, err)
	}

	var imagePaths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
				imagePaths = append(imagePaths, filepath.Join(inputDir, entry.Name()))
			}
		}
	}

	if len(imagePaths) == 0 {
		b.Fatalf("No sample images found in %s", inputDir)
	}

	// 2. Initialize in-memory SQLite DB
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		b.Fatalf("Failed to open test database: %v", err)
	}
	defer db.Close()

	// Initialize the photos table schema matching your app
	_, err = db.Exec(`
		CREATE TABLE photos (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			file_path TEXT UNIQUE,
			file_name TEXT,
			file_hash TEXT,
			file_size INTEGER,
			width INTEGER,
			height INTEGER,
			date_taken DATETIME,
			date_modified DATETIME,
			camera_make TEXT,
			camera_model TEXT
		);
	`)
	if err != nil {
		b.Fatalf("Failed to create test schema: %v", err)
	}

	// 3. Inject mock state
	oldDB := state.DB
	oldDataDir := state.Config.DataDir

	state.DB = db
	state.Config.DataDir = testOutputDir

	defer func() {
		state.DB = oldDB
		state.Config.DataDir = oldDataDir
	}()

	// 4. Clean and recreate output directory on each run
	_ = os.RemoveAll(smallThumbDir)
	if err := os.MkdirAll(smallThumbDir, 0755); err != nil {
		b.Fatalf("Failed to create thumbnail directory (%s): %v", smallThumbDir, err)
	}

	// Reset timer to measure only processing time
	b.ResetTimer()

	// 5. Run benchmark
	for i := 0; i < b.N; i++ {
		for _, imgPath := range imagePaths {
			err := processImage(imgPath)
			if err != nil {
				b.Errorf("processImage failed for %s: %v", imgPath, err)
			}
		}
	}
}
