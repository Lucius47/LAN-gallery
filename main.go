package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/rwcarlsen/goexif/exif"
	_ "modernc.org/sqlite"

	_ "net/http/pprof"
)

// Config holds runtime configuration settings.
type Config struct {
	Port           int      `json:"port"`
	DataDir        string   `json:"data_dir"`
	PhotoDirs      []string `json:"photo_dirs"`
	WorkerPoolSize int      `json:"worker_pool_size"`
}

// Photo represents an indexed image file in the gallery.
type Photo struct {
	ID           int64     `json:"id"`
	FilePath     string    `json:"file_path"`
	FileName     string    `json:"file_name"`
	FileHash     string    `json:"file_hash"`
	FileSize     int64     `json:"file_size"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	DateTaken    time.Time `json:"date_taken"`
	DateModified time.Time `json:"date_modified"`
	CameraMake   string    `json:"camera_make,omitempty"`
	CameraModel  string    `json:"camera_model,omitempty"`
	Tags         []Tag     `json:"tags"`
	URLs         PhotoURLs `json:"urls"`
}

type PhotoURLs struct {
	Thumbnail string `json:"thumbnail"`
	Preview   string `json:"preview"`
	Original  string `json:"original"`
}

// Tag represents a user-defined category or metadata label.
type Tag struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

// SystemStatus holds runtime statistics for monitoring.
type SystemStatus struct {
	TotalPhotos    int64    `json:"total_photos"`
	TotalTags      int64    `json:"total_tags"`
	DbSizeBytes    int64    `json:"db_size_bytes"`
	CacheSizeBytes int64    `json:"cache_size_bytes"`
	IsScanning     bool     `json:"is_scanning"`
	MonitoredDirs  []string `json:"monitored_dirs"`
}

type AppState struct {
	Config     Config
	DB         *sql.DB
	ScanLock   sync.Mutex
	IsScanning bool
}

var state AppState

func initConfig() {
	state.Config = Config{
		Port:           8080,
		DataDir:        "./data",
		PhotoDirs:      []string{},
		WorkerPoolSize: 4,
	}

	configFile := "config.json"
	if data, err := os.ReadFile(configFile); err == nil {
		_ = json.Unmarshal(data, &state.Config)
	} else {
		saveConfig()
	}

	// Create internal data directories
	_ = os.MkdirAll(filepath.Join(state.Config.DataDir, "cache", "small"), 0755)
	_ = os.MkdirAll(filepath.Join(state.Config.DataDir, "cache", "medium"), 0755)
}

func saveConfig() {
	data, _ := json.MarshalIndent(state.Config, "", "  ")
	_ = os.WriteFile("config.json", data, 0644)
}

func initDB() {
	dbPath := filepath.Join(state.Config.DataDir, "gallery.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		log.Fatalf("Failed to open SQLite database: %v", err)
	}

	state.DB = db

	schema := `
	CREATE TABLE IF NOT EXISTS photos (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		file_path TEXT NOT NULL UNIQUE,
		file_name TEXT NOT NULL,
		file_hash TEXT NOT NULL,
		file_size INTEGER NOT NULL,
		width INTEGER NOT NULL,
		height INTEGER NOT NULL,
		date_taken DATETIME,
		date_modified DATETIME NOT NULL,
		camera_make TEXT,
		camera_model TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_photos_date_taken ON photos(date_taken DESC);
	CREATE INDEX IF NOT EXISTS idx_photos_file_hash ON photos(file_hash);

	CREATE TABLE IF NOT EXISTS tags (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE COLLATE NOCASE,
		category TEXT DEFAULT 'general',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_tags_name ON tags(name);

	CREATE TABLE IF NOT EXISTS photo_tags (
		photo_id INTEGER NOT NULL,
		tag_id INTEGER NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (photo_id, tag_id),
		FOREIGN KEY (photo_id) REFERENCES photos(id) ON DELETE CASCADE,
		FOREIGN KEY (tag_id) REFERENCES tags(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_photo_tags_tag ON photo_tags(tag_id);
	`

	if _, err := db.Exec(schema); err != nil {
		log.Fatalf("Failed to initialize database schema: %v", err)
	}

	log.Println("Database connection established with WAL mode enabled.")
}

// calculateFastHash generates a hash digest using path, size, and modification time.
func calculateFastHash(path string, size int64, modTime time.Time) string {
	h := sha256.New()
	io.WriteString(h, fmt.Sprintf("%s|%d|%d", path, size, modTime.Unix()))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// processImage extracts EXIF metadata and builds multi-tier thumbnail caches.
func processImage(filePath string) error {
	info, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	hash := calculateFastHash(filePath, info.Size(), info.ModTime())

	// Check if already indexed in SQLite
	var existingID int64
	err = state.DB.QueryRow("SELECT id FROM photos WHERE file_hash = ?", hash).Scan(&existingID)
	if err == nil {
		return nil // File exists and has not changed
	}

	// Open image file for EXIF extraction
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	dateTaken := info.ModTime()
	var cameraMake, cameraModel string
	var exifThumb []byte

	// Extract EXIF data
	x, err := exif.Decode(file)
	if err == nil {
		if dt, err := x.DateTime(); err == nil {
			dateTaken = dt
		}
		if makeTag, err := x.Get(exif.Make); err == nil {
			cameraMake, _ = makeTag.StringVal()
		}
		if modelTag, err := x.Get(exif.Model); err == nil {
			cameraModel, _ = modelTag.StringVal()
		}
		// Try extracting embedded thumbnail bytes
		if thumbBytes, err := x.JpegThumbnail(); err == nil && len(thumbBytes) > 0 {
			exifThumb = thumbBytes
		}
	}

	// Extract ORIGINAL photo dimensions (fast header read + orientation check)
	width, height, _ := getOrientedDimensions(file, x)

	smallThumbPath := filepath.Join(state.Config.DataDir, "cache", "small", hash+".jpg")

	// FAST PATH: Write embedded thumbnail to disk if available
	if len(exifThumb) > 0 {
		if _, err := os.Stat(smallThumbPath); os.IsNotExist(err) {
			_ = os.WriteFile(smallThumbPath, exifThumb, 0644)
		}
	}

	// SLOW FALLBACK PATH: If no EXIF thumbnail exists (or dimensions failed), decode full image
	if len(exifThumb) == 0 || width == 0 || height == 0 {
		img, err := imaging.Open(filePath, imaging.AutoOrientation(true))
		if err != nil {
			return fmt.Errorf("failed to decode image: %w", err)
		}

		bounds := img.Bounds()
		width, height = bounds.Dx(), bounds.Dy()

		// Generate Small Grid Thumbnail (250px)
		if _, err := os.Stat(smallThumbPath); os.IsNotExist(err) {
			smallImg := imaging.Fit(img, 250, 250, imaging.Lanczos)
			_ = imaging.Save(smallImg, smallThumbPath, imaging.JPEGQuality(80))
		}
	}

	// Insert into SQLite
	_, err = state.DB.Exec(`
		INSERT INTO photos (file_path, file_name, file_hash, file_size, width, height, date_taken, date_modified, camera_make, camera_model)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			file_hash=excluded.file_hash,
			file_size=excluded.file_size,
			width=excluded.width,
			height=excluded.height,
			date_taken=excluded.date_taken,
			date_modified=excluded.date_modified;
	`, filePath, filepath.Base(filePath), hash, info.Size(), width, height, dateTaken, info.ModTime(), strings.TrimSpace(cameraMake), strings.TrimSpace(cameraModel))

	return err
}

// Helper to extract true oriented dimensions without pixel decoding
func getOrientedDimensions(file *os.File, x *exif.Exif) (int, int, error) {
	_, _ = file.Seek(0, io.SeekStart)
	cfg, _, err := image.DecodeConfig(file)
	if err != nil {
		return 0, 0, err
	}

	w, h := cfg.Width, cfg.Height

	if x != nil {
		if orientTag, err := x.Get(exif.Orientation); err == nil {
			if val, err := orientTag.Int(0); err == nil {
				if val >= 5 && val <= 8 {
					w, h = h, w // Swap width and height for 90/270 rotated orientations
				}
			}
		}
	}

	return w, h, nil
}

func startDirectoryScan() {
	state.ScanLock.Lock()
	if state.IsScanning {
		state.ScanLock.Unlock()
		return
	}
	state.IsScanning = true
	state.ScanLock.Unlock()

	go func() {
		defer func() {
			state.ScanLock.Lock()
			state.IsScanning = false
			state.ScanLock.Unlock()
		}()

		log.Println("Starting background directory scan...")

		for _, dir := range state.Config.PhotoDirs {
			_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}

				ext := strings.ToLower(filepath.Ext(path))
				if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" {
					if procErr := processImage(path); procErr != nil {
						log.Printf("Error processing %s: %v\n", path, procErr)
					}
				}
				return nil
			})
		}

		log.Println("Directory scan complete.")
	}()
}

func enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func respondJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{"error": message})
}

func handleGetPhotos(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 500 {
		limit = 60
	}
	offset := (page - 1) * limit

	tagFilter := r.URL.Query().Get("tags")

	var query string
	var args []interface{}

	if tagFilter != "" {
		tagsList := strings.Split(tagFilter, ",")
		placeholders := make([]string, len(tagsList))
		for i, t := range tagsList {
			placeholders[i] = "?"
			args = append(args, strings.TrimSpace(t))
		}

		query = fmt.Sprintf(`
			SELECT DISTINCT p.id, p.file_path, p.file_name, p.file_hash, p.file_size, p.width, p.height, p.date_taken, p.date_modified, p.camera_make, p.camera_model
			FROM photos p
			JOIN photo_tags pt ON p.id = pt.photo_id
			JOIN tags t ON pt.tag_id = t.id
			WHERE t.name IN (%s)
			ORDER BY p.date_taken DESC
			LIMIT ? OFFSET ?
		`, strings.Join(placeholders, ","))

		args = append(args, limit, offset)
	} else {
		query = `
			SELECT id, file_path, file_name, file_hash, file_size, width, height, date_taken, date_modified, camera_make, camera_model
			FROM photos
			ORDER BY date_taken DESC
			LIMIT ? OFFSET ?
		`
		args = append(args, limit, offset)
	}

	rows, err := state.DB.Query(query, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to query database")
		return
	}
	defer rows.Close()

	var photos []Photo
	for rows.Next() {
		var p Photo
		var dateTaken, dateModified time.Time
		var make, model sql.NullString

		err := rows.Scan(&p.ID, &p.FilePath, &p.FileName, &p.FileHash, &p.FileSize, &p.Width, &p.Height, &dateTaken, &dateModified, &make, &model)
		if err != nil {
			continue
		}

		p.DateTaken = dateTaken
		p.DateModified = dateModified
		p.CameraMake = make.String
		p.CameraModel = model.String
		p.URLs = PhotoURLs{
			Thumbnail: fmt.Sprintf("/api/v1/photos/%d/asset?type=small", p.ID),
			Preview:   fmt.Sprintf("/api/v1/photos/%d/asset?type=medium", p.ID),
			Original:  fmt.Sprintf("/api/v1/photos/%d/asset?type=original", p.ID),
		}

		// Attach tags
		p.Tags = getPhotoTags(p.ID)

		photos = append(photos, p)
	}

	var total int64
	_ = state.DB.QueryRow("SELECT COUNT(*) FROM photos").Scan(&total)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"total": total,
		"page":  page,
		"limit": limit,
		"data":  photos,
	})
}

func getPhotoTags(photoID int64) []Tag {
	rows, err := state.DB.Query(`
		SELECT t.id, t.name, t.category
		FROM tags t
		JOIN photo_tags pt ON t.id = pt.tag_id
		WHERE pt.photo_id = ?
	`, photoID)
	if err != nil {
		return []Tag{}
	}
	defer rows.Close()

	var tags []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Category); err == nil {
			tags = append(tags, t)
		}
	}
	return tags
}

func handleGetAsset(w http.ResponseWriter, r *http.Request) {
	// Extract photo ID from URL path
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		respondError(w, http.StatusBadRequest, "Invalid asset request")
		return
	}

	id, _ := strconv.ParseInt(parts[4], 10, 64)
	assetType := r.URL.Query().Get("type")

	var filePath, fileHash string
	err := state.DB.QueryRow("SELECT file_path, file_hash FROM photos WHERE id = ?", id).Scan(&filePath, &fileHash)
	if err != nil {
		respondError(w, http.StatusNotFound, "Photo not found")
		return
	}

	var targetPath string
	switch assetType {
	case "medium":
		// Medium thumbnail cache disabled; fall back to small thumbnail
		targetPath = filepath.Join(state.Config.DataDir, "cache", "small", fileHash+".jpg")
	case "original":
		targetPath = filePath
	default: // small thumbnail
		targetPath = filepath.Join(state.Config.DataDir, "cache", "small", fileHash+".jpg")
	}

	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		// Fallback to original if cache doesn't exist yet
		targetPath = filePath
	}

	// Cache static assets aggressively
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, targetPath)
}

func handleTags(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		rows, err := state.DB.Query("SELECT id, name, category FROM tags ORDER BY name ASC")
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to query tags")
			return
		}
		defer rows.Close()

		var tags []Tag
		for rows.Next() {
			var t Tag
			_ = rows.Scan(&t.ID, &t.Name, &t.Category)
			tags = append(tags, t)
		}
		respondJSON(w, http.StatusOK, tags)

	case "POST":
		var req struct {
			Name     string `json:"name"`
			Category string `json:"category"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
			respondError(w, http.StatusBadRequest, "Invalid tag payload")
			return
		}

		if req.Category == "" {
			req.Category = "general"
		}

		res, err := state.DB.Exec("INSERT INTO tags (name, category) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET category=excluded.category", strings.TrimSpace(req.Name), req.Category)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to create tag")
			return
		}

		id, _ := res.LastInsertId()
		respondJSON(w, http.StatusCreated, Tag{ID: id, Name: req.Name, Category: req.Category})
	}
}

func handlePhotoTagAssociation(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		respondError(w, http.StatusBadRequest, "Invalid URL path")
		return
	}

	photoID, _ := strconv.ParseInt(parts[4], 10, 64)

	if r.Method == "POST" {
		var req struct {
			TagID int64 `json:"tag_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "Invalid JSON payload")
			return
		}

		_, err := state.DB.Exec("INSERT OR IGNORE INTO photo_tags (photo_id, tag_id) VALUES (?, ?)", photoID, req.TagID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to attach tag")
			return
		}

		respondJSON(w, http.StatusOK, map[string]string{"status": "attached"})
	} else if r.Method == "DELETE" {
		if len(parts) < 7 {
			respondError(w, http.StatusBadRequest, "Missing tag_id in path")
			return
		}
		tagID, _ := strconv.ParseInt(parts[6], 10, 64)

		_, err := state.DB.Exec("DELETE FROM photo_tags WHERE photo_id = ? AND tag_id = ?", photoID, tagID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to detach tag")
			return
		}

		respondJSON(w, http.StatusOK, map[string]string{"status": "detached"})
	}
}

func handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	var photoCount, tagCount int64
	_ = state.DB.QueryRow("SELECT COUNT(*) FROM photos").Scan(&photoCount)
	_ = state.DB.QueryRow("SELECT COUNT(*) FROM tags").Scan(&tagCount)

	dbPath := filepath.Join(state.Config.DataDir, "gallery.db")
	dbInfo, _ := os.Stat(dbPath)
	var dbSize int64
	if dbInfo != nil {
		dbSize = dbInfo.Size()
	}

	var cacheSize int64
	_ = filepath.Walk(filepath.Join(state.Config.DataDir, "cache"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			cacheSize += info.Size()
		}
		return nil
	})

	state.ScanLock.Lock()
	isScanning := state.IsScanning
	state.ScanLock.Unlock()

	respondJSON(w, http.StatusOK, SystemStatus{
		TotalPhotos:    photoCount,
		TotalTags:      tagCount,
		DbSizeBytes:    dbSize,
		CacheSizeBytes: cacheSize,
		IsScanning:     isScanning,
		MonitoredDirs:  state.Config.PhotoDirs,
	})
}

func handleSystemDirectories(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var req struct {
			Directory string `json:"directory"`
			Action    string `json:"action"` // "add" or "remove"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Directory == "" {
			respondError(w, http.StatusBadRequest, "Invalid request payload")
			return
		}

		req.Directory = filepath.Clean(req.Directory)

		if req.Action == "add" {
			exists := false
			for _, d := range state.Config.PhotoDirs {
				if d == req.Directory {
					exists = true
					break
				}
			}
			if !exists {
				state.Config.PhotoDirs = append(state.Config.PhotoDirs, req.Directory)
				saveConfig()
				startDirectoryScan()
			}
		} else if req.Action == "remove" {
			newDirs := []string{}
			for _, d := range state.Config.PhotoDirs {
				if d != req.Directory {
					newDirs = append(newDirs, d)
				}
			}
			state.Config.PhotoDirs = newDirs
			saveConfig()

			// Clean up indexed photos and cached thumbnails associated with this directory
			go pruneDirectoryFromDB(req.Directory)

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "removed",
				"message": "Directory removed from config and background pruning initiated.",
			})
			return
		}

		respondJSON(w, http.StatusOK, state.Config.PhotoDirs)
	} else {
		respondJSON(w, http.StatusOK, state.Config.PhotoDirs)
	}
}

// Helper to remove DB records and cached thumbnails when a directory is un-monitored
func pruneDirectoryFromDB(dirPath string) {
	cleanDir := filepath.Clean(dirPath)
	targetPrefix := strings.ToLower(cleanDir)
	if !strings.HasSuffix(targetPrefix, string(os.PathSeparator)) {
		targetPrefix += string(os.PathSeparator)
	}

	// 1. Query all photos to match paths reliably in Go (cross-platform & Windows slash-safe)
	rows, err := state.DB.Query("SELECT id, file_path, file_hash FROM photos")
	if err != nil {
		log.Printf("[Prune Error] Failed to query photos for pruning: %v", err)
		return
	}
	defer rows.Close()

	var idsToDelete []int
	var hashesToDelete []string

	for rows.Next() {
		var id int
		var filePath, hash string
		if err := rows.Scan(&id, &filePath, &hash); err == nil {
			cleanPhotoPath := strings.ToLower(filepath.Clean(filePath))
			// Check if the photo path starts with the un-monitored directory path
			if strings.HasPrefix(cleanPhotoPath, targetPrefix) || cleanPhotoPath == strings.ToLower(cleanDir) {
				idsToDelete = append(idsToDelete, id)
				hashesToDelete = append(hashesToDelete, hash)
			}
		}
	}

	// 2. Delete cached thumbnail files (both small and medium if present)
	deletedThumbs := 0
	for _, hash := range hashesToDelete {
		smallThumb := filepath.Join("data", "cache", "small", hash+".webp") // TOFIX: Ensure consistent thumbnail extension
		if err := os.Remove(smallThumb); err == nil {
			deletedThumbs++
		}
		mediumThumb := filepath.Join("data", "cache", "medium", hash+".webp")
		_ = os.Remove(mediumThumb)
	}

	// 3. Delete DB entries in a transaction
	if len(idsToDelete) > 0 {
		tx, err := state.DB.Begin()
		if err == nil {
			stmt, err := tx.Prepare("DELETE FROM photos WHERE id = ?")
			if err == nil {
				defer stmt.Close()
				for _, id := range idsToDelete {
					_, _ = stmt.Exec(id)
				}
				_ = tx.Commit()
			}
		}
	}

	log.Printf("[Prune] Removed %d database records and %d cached thumbnails for directory: %s", len(idsToDelete), deletedThumbs, dirPath)
}

func handleSystemTriggerScan(w http.ResponseWriter, r *http.Request) {
	startDirectoryScan()
	respondJSON(w, http.StatusOK, map[string]string{"message": "Scan initiated"})
}

func handleSystemPurgeCache(w http.ResponseWriter, r *http.Request) {
	_ = os.RemoveAll(filepath.Join(state.Config.DataDir, "cache"))
	_ = os.MkdirAll(filepath.Join(state.Config.DataDir, "cache", "small"), 0755)
	_ = os.MkdirAll(filepath.Join(state.Config.DataDir, "cache", "medium"), 0755)

	respondJSON(w, http.StatusOK, map[string]string{"message": "Cache purged successfully"})
}

func main() {

	// Start a separate HTTP server for profiling data or expose it on your main router
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	initConfig()
	initDB()

	mux := http.NewServeMux()

	// API Routes
	mux.HandleFunc("/api/v1/photos", handleGetPhotos)
	mux.HandleFunc("/api/v1/photos/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/asset") {
			handleGetAsset(w, r)
		} else if strings.Contains(r.URL.Path, "/tags") {
			handlePhotoTagAssociation(w, r)
		} else {
			respondError(w, http.StatusNotFound, "Endpoint not found")
		}
	})

	mux.HandleFunc("/api/v1/tags", handleTags)
	mux.HandleFunc("/api/v1/system/status", handleSystemStatus)
	mux.HandleFunc("/api/v1/system/directories", handleSystemDirectories)
	mux.HandleFunc("/api/v1/system/scan", handleSystemTriggerScan)
	mux.HandleFunc("/api/v1/system/purge-cache", handleSystemPurgeCache)

	// Dedicated /hello endpoint to quickly verify network connectivity
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "online",
			"message": "Hello! Local Photo Gallery server is online and accessible over network.",
			"port":    state.Config.Port,
		})
	})

	// Serve static web UI (index.html) at root route, or a hello message if index.html is missing
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if _, err := os.Stat("index.html"); err == nil {
			http.ServeFile(w, r, "index.html")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "online",
			"message": "Hello! Local Photo Gallery server is up and running.",
			"port":    state.Config.Port,
			"hint":    "Place 'index.html' in the same directory as main.go to serve the web UI directly.",
		})
	})

	handler := enableCORS(mux)

	serverAddr := fmt.Sprintf("0.0.0.0:%d", state.Config.Port)
	log.Printf("Local Photo Gallery server running on http://%s\n", serverAddr)
	log.Printf("Ready for incoming LAN connections across local devices.\n")

	if err := http.ListenAndServe(serverAddr, handler); err != nil {
		log.Fatalf("Server shutdown with error: %v", err)
	}
}
