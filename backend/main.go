package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

type Video struct {
	ID           string  `json:"id"`
	Path         string  `json:"path"`
	Resolution   string  `json:"resolution"`
	Bitrate      string  `json:"bitrate"`
	Status       string  `json:"status"`
	OriginalSize int64   `json:"originalSize"`
}

type VideoQueue struct {
	Pending    []Video `json:"pending"`
	Processing []Video `json:"processing"`
	Completed  []Video `json:"completed"`
	Total      int     `json:"total"`
}

type PaginationParams struct {
	Page     int    `form:"page,default=1"`
	PageSize int    `form:"pageSize,default=10"`
	Status   string `form:"status"`
}

var db *sql.DB
var processedPaths map[string]bool

func initDB() {
	var err error
	dbURL := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		os.Getenv("DB_HOST"),
		os.Getenv("DB_PORT"),
		os.Getenv("DB_USER"),
		os.Getenv("DB_PASSWORD"),
		os.Getenv("DB_NAME"),
		//"localhost",
		//"5432",
		//"postgres",
		//"postgres",
		//"videoqueue",
	)
	
	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal(err)
	}

	// Create videos table if not exists
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS videos (
			id TEXT PRIMARY KEY,
			path TEXT NOT NULL,
			resolution TEXT,
			bitrate TEXT,
			status TEXT NOT NULL,
			original_size BIGINT NOT NULL
		)
	`)
	if err != nil {
		log.Fatal(err)
	}

	// Create index on path and status
	_, err = db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_videos_path ON videos(path);
		CREATE INDEX IF NOT EXISTS idx_videos_status ON videos(status);
	`)
	if err != nil {
		log.Fatal(err)
	}

	// Initialize processed paths cache
	refreshProcessedPathsCache()
}

func refreshProcessedPathsCache() {
	processedPaths = make(map[string]bool)
	rows, err := db.Query("SELECT path FROM videos")
	if err != nil {
		log.Printf("Error refreshing paths cache: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			log.Printf("Error scanning path: %v", err)
			continue
		}
		processedPaths[path] = true
	}
}

func getUnprocessedVideos(c *gin.Context) {
	videos := []Video{}
	videosDir := "./videos"

	// Get all video paths first
	var videoPaths []string
	err := filepath.Walk(videosDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ".mp4" {
			videoPaths = append(videoPaths, path)
		}
		return nil
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Filter unprocessed videos using the cache
	for _, path := range videoPaths {
		if !processedPaths[path] {
			video := Video{
				ID:           filepath.Base(path),
				Path:         path,
				Status:       "unprocessed",
				OriginalSize: getFileSize(path),
			}
			videos = append(videos, video)
		}
	}

	c.JSON(http.StatusOK, videos)
}

func getFileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func getQueue(c *gin.Context) {
	var params PaginationParams
	if err := c.ShouldBindQuery(&params); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate and set defaults
	if params.Page < 1 {
		params.Page = 1
	}
	if params.PageSize < 1 || params.PageSize > 100 {
		params.PageSize = 10
	}

	offset := (params.Page - 1) * params.PageSize

	// Build query based on status filter
	baseQuery := "SELECT id, path, resolution, bitrate, status, original_size FROM videos"
	countQuery := "SELECT COUNT(*) FROM videos"
	var whereClause string

	if params.Status != "" {
		whereClause = fmt.Sprintf(" WHERE status = '%s'", params.Status)
	}

	// Get total count
	var total int
	err := db.QueryRow(countQuery + whereClause).Scan(&total)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Get paginated results
	query := baseQuery + whereClause + 
		" ORDER BY CASE status " +
		"WHEN 'processing' THEN 1 " +
		"WHEN 'pending' THEN 2 " +
		"WHEN 'completed' THEN 3 " +
		"ELSE 4 END, path" +
		fmt.Sprintf(" LIMIT %d OFFSET %d", params.PageSize, offset)

	rows, err := db.Query(query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	videos := scanVideoRows(rows)

	// Group videos by status
	queue := VideoQueue{
		Pending:    []Video{},
		Processing: []Video{},
		Completed:  []Video{},
		Total:      total,
	}

	for _, video := range videos {
		switch video.Status {
		case "pending":
			queue.Pending = append(queue.Pending, video)
		case "processing":
			queue.Processing = append(queue.Processing, video)
		case "completed":
			queue.Completed = append(queue.Completed, video)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"queue": queue,
		"pagination": gin.H{
			"page":     params.Page,
			"pageSize": params.PageSize,
			"total":    total,
			"pages":    (total + params.PageSize - 1) / params.PageSize,
		},
	})
}

func scanVideoRows(rows *sql.Rows) []Video {
	var videos []Video
	for rows.Next() {
		var v Video
		err := rows.Scan(&v.ID, &v.Path, &v.Resolution, &v.Bitrate, &v.Status, &v.OriginalSize)
		if err != nil {
			log.Printf("Error scanning video row: %v", err)
			continue
		}
		videos = append(videos, v)
	}
	return videos
}

func addToQueue(c *gin.Context) {
	var video Video
	if err := c.ShouldBindJSON(&video); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Insert video into database
	_, err := db.Exec(`
		INSERT INTO videos (id, path, resolution, bitrate, status, original_size)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, video.ID, video.Path, video.Resolution, video.Bitrate, "pending", video.OriginalSize)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Update processed paths cache
	processedPaths[video.Path] = true

	c.JSON(http.StatusOK, video)
}

func updateVideoStatus(c *gin.Context) {
	id := c.Param("id")
	var statusUpdate struct {
		Status string `json:"status"`
	}

	if err := c.ShouldBindJSON(&statusUpdate); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Update video status in database
	_, err := db.Exec("UPDATE videos SET status = $1 WHERE id = $2", statusUpdate.Status, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "updated"})
}

func main() {
	// Initialize database connection
	initDB()
	defer db.Close()

	r := gin.Default()

	// Configure CORS
	config := cors.DefaultConfig()
	config.AllowOrigins = []string{"*"}
	config.AllowMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	config.AllowHeaders = []string{"Origin", "Content-Type"}
	
	r.Use(cors.New(config))

	// Routes
	r.GET("/api/videos/unprocessed", getUnprocessedVideos)
	r.GET("/api/queue", getQueue)
	r.POST("/api/videos/process", addToQueue)
	r.PATCH("/api/videos/:id/status", updateVideoStatus)

	r.Run(":8080")
}
