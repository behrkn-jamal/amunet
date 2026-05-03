package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

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

func main() {
	initDB()

	r := gin.Default()

	// Endpoint: List Categories
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

	// Endpoint: List Tattoos
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
			// Parse JSONB metadata
			importJSON(metadata, &td.Metadata)
			tattoos = append(tattoos, td)
		}
		c.JSON(http.StatusOK, gin.H{"data": tattoos})
	})

	// Endpoint: Single Tattoo
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

// Remove the placeholder helper functions

