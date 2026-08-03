package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// BenchmarkProcessImage benchmarks the actual processImage function directly.
func BenchmarkProcessImage(b *testing.B) {
	// 1. Set up paths relative to your project root or test data
	inputDir := "./tests/testdata/images"
	cacheDir := b.TempDir() // Go automatically creates and cleans up this temp dir

	// Read image file paths
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		b.Fatalf("Failed to read test image directory: %v", err)
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

	// 2. Initialize an in-memory SQLite DB for testing
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

	// 3. Inject mock/test state into your global app state
	// (Adjust this to match your actual 'state' struct setup)
	oldDB := state.DB
	oldDataDir := state.Config.DataDir

	state.DB = db
	state.Config.DataDir = cacheDir

	// Ensure subdirectories exist for thumbnail cache
	_ = os.MkdirAll(filepath.Join(cacheDir, "cache", "small"), 0755)

	// Restore original state when benchmark finishes
	defer func() {
		state.DB = oldDB
		state.Config.DataDir = oldDataDir
	}()

	// Reset timer to exclude directory reading & DB schema initialization
	b.ResetTimer()

	// 4. Run the benchmark loop
	for i := 0; i < b.N; i++ {
		for _, imgPath := range imagePaths {
			err := processImage(imgPath)
			if err != nil {
				b.Errorf("processImage failed for %s: %v", imgPath, err)
			}
		}
	}
}

// go test -bench=BenchmarkProcessImage -run=^$ -benchmem
