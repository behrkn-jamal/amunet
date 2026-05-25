package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

type TattooDesign struct {
	ID       string                 `json:"id"`
	Name     string                 `json:"name"`
	Category string                 `json:"category"`
	ImageURL string                 `json:"image_url"`
	Metadata map[string]interface{} `json:"metadata"`
	IsCustom bool                   `json:"is_custom"`
}

type Category struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

var db *sql.DB
var s3Client *s3.S3
var uploader *s3manager.Uploader

func initDB() {
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		os.Getenv("DB_HOST"), os.Getenv("DB_PORT"), os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD"), os.Getenv("DB_NAME"))

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal(err)
	}

	if err = db.Ping(); err != nil {
		log.Fatal("Cannot connect to database: ", err)
	}
	fmt.Println("Connected to PostgreSQL successfully")
}

func initR2() {
	sess, err := session.NewSession(&aws.Config{
		Credentials: credentials.NewStaticCredentials(
			os.Getenv("R2_ACCESS_KEY_ID"),
			os.Getenv("R2_SECRET_ACCESS_KEY"),
			"",
		),
		Endpoint: aws.String(os.Getenv("R2_S3_ENDPOINT")),
		Region:   aws.String("auto"),
	})
	if err != nil {
		log.Fatal("Failed to create R2 session: ", err)
	}

	s3Client = s3.New(sess)
	uploader = s3manager.NewUploader(sess)
	fmt.Println("Connected to Cloudflare R2 successfully")
}

func randStringRunes(n int) string {
	letterRunes := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

func main() {
	// Seed random number generator
	rand.Seed(time.Now().UnixNano())

	initDB()
	initR2()

	r := gin.Default()

	// --- PUBLIC ENDPOINTS ---

	// List Categories
	r.GET("/categories", func(c *gin.Context) {
		rows, err := db.Query("SELECT id, name FROM categories ORDER BY name ASC")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		categories := []Category{}
		for rows.Next() {
			var cat Category
			if err := rows.Scan(&cat.ID, &cat.Name); err != nil {
				log.Println(err)
				continue
			}
			categories = append(categories, cat)
		}
		c.JSON(http.StatusOK, gin.H{"categories": categories})
	})

	// List Tattoos
	r.GET("/tattoos", func(c *gin.Context) {
		categoryFilter := c.Query("category")
		var query string
		var args []interface{}

		if categoryFilter != "" {
			query = `SELECT t.id, t.name, c.name, t.image_url, t.metadata, t.is_custom 
				FROM tattoos t JOIN categories c ON t.category_id = c.id 
				WHERE c.name = $1`
			args = append(args, categoryFilter)
		} else {
			query = `SELECT t.id, t.name, c.name, t.image_url, t.metadata, t.is_custom 
				FROM tattoos t JOIN categories c ON t.category_id = c.id`
		}

		rows, err := db.Query(query, args...)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		tattoos := []TattooDesign{}
		for rows.Next() {
			var td TattooDesign
			var metadata []byte
			if err := rows.Scan(&td.ID, &td.Name, &td.Category, &td.ImageURL, &metadata, &td.IsCustom); err != nil {
				log.Println(err)
				continue
			}
			importJSON(metadata, &td.Metadata)
			tattoos = append(tattoos, td)
		}
		c.JSON(http.StatusOK, gin.H{"data": tattoos})
	})

	// Single Tattoo
	r.GET("/tattoos/:id", func(c *gin.Context) {
		id := c.Param("id")
		var td TattooDesign
		var metadata []byte

		query := `SELECT t.id, t.name, c.name, t.image_url, t.metadata, t.is_custom 
			FROM tattoos t JOIN categories c ON t.category_id = c.id 
			WHERE t.id = $1`

		err := db.QueryRow(query, id).Scan(&td.ID, &td.Name, &td.Category, &td.ImageURL, &metadata, &td.IsCustom)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Tattoo design not found"})
			return
		}
		importJSON(metadata, &td.Metadata)
		c.JSON(http.StatusOK, td)
	})

	// --- ADMIN ENDPOINTS ---

	// Upload Tattoo
	r.POST("/admin/tattoos/upload", func(c *gin.Context) {
		// 1. Auth check
		apiKey := c.GetHeader("X-Admin-API-Key")
		if apiKey == "" || apiKey != os.Getenv("ADMIN_API_KEY") {
			c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "message": "Invalid or missing API Key"})
			return
		}

		// 2. Parse form
		file, header, err := c.Request.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "File is required"})
			return
		}
		defer file.Close()

		name := c.PostForm("name")
		category := c.PostForm("category")
		metadataStr := c.PostForm("metadata")

		if name == "" || category == "" {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Name and category are required"})
			return
		}

		// 3. Generate ID and Path
		designID := fmt.Sprintf("%s_%d_%s", category, time.Now().UnixNano(), randStringRunes(6))
		extension := filepath.Ext(header.Filename)
		r2Path := fmt.Sprintf("tattoos/%s/%s%s", category, designID, extension)

		// 4. Upload to R2
		_, err = uploader.Upload(&s3manager.UploadInput{
			Bucket: aws.String(os.Getenv("R2_BUCKET_NAME")),
			Key:    aws.String(r2Path),
			Body:   file,
			ContentType: aws.String("image/png"),
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Upload to R2 failed: " + err.Error()})
			return
		}

		// 5. Get the public URL
		publicURL := fmt.Sprintf("%s/%s", os.Getenv("CDN_DOMAIN"), r2Path)

		// 6. Save to DB
		var metadataJSON []byte
		if metadataStr != "" {
			var metadata map[string]interface{}
			if err := json.Unmarshal([]byte(metadataStr), &metadata); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid metadata JSON: " + err.Error()})
				return
			}
			metadataJSON, err = json.Marshal(metadata)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Metadata serialization failed: " + err.Error()})
				return
			}
		}

		// Find category ID
		var categoryID int
		err = db.QueryRow("SELECT id FROM categories WHERE name = $1", category).Scan(&categoryID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Category not found in DB"})
			return
		}

		_, err = db.Exec("INSERT INTO tattoos (id, name, category_id, image_url, metadata) VALUES ($1, $2, $3, $4, $5)",
			designID, name, categoryID, publicURL, metadataJSON)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "DB update failed: " + err.Error()})
			return
		}

		c.JSON(http.StatusCreated, gin.H{
			"status": "success",
			"data": gin.H{
				"id": designID,
				"name": name,
				"category": category,
				"image_url": publicURL,
				"created_at": "now",
			},
		})
	})

	r.Run(":8080")
}

func importJSON(data []byte, target *map[string]interface{}) {
	if len(data) == 0 {
		return
	}
	if err := json.Unmarshal(data, target); err != nil {
		log.Printf("Error unmarshaling JSON: %v", err)
	}
}