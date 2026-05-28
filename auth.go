package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// --- Types ---

type AdminUser struct {
	ID           int       `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type LoginResponse struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in"` // seconds
	Username  string `json:"username"`
}

// --- Table Init ---

func initAdminTable() error {
	// Create admin_users table
	query := `
	CREATE TABLE IF NOT EXISTS admin_users (
		id SERIAL PRIMARY KEY,
		username VARCHAR(255) UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
		updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
	);
	`
	_, err := db.Exec(query)
	if err != nil {
		return fmt.Errorf("create admin_users table: %w", err)
	}

	// Create index on username
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_admin_users_username ON admin_users(username)")
	if err != nil {
		return fmt.Errorf("create username index: %w", err)
	}

	// Create token blacklist table for logout
	query = `
	CREATE TABLE IF NOT EXISTS token_blacklist (
		id SERIAL PRIMARY KEY,
		token_jti VARCHAR(64) UNIQUE NOT NULL,
		expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
	);
	`
	_, err = db.Exec(query)
	if err != nil {
		return fmt.Errorf("create token_blacklist table: %w", err)
	}

	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_token_blacklist_jti ON token_blacklist(token_jti)")
	if err != nil {
		return fmt.Errorf("create blacklist index: %w", err)
	}

	return nil
}

// Seed a default admin user if none exists.
// Admin credentials are configured via env vars:
//   ADMIN_USERNAME (default: "admin")
//   ADMIN_PASSWORD (default: "admin123")
func seedDefaultAdmin() error {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM admin_users").Scan(&count)
	if err != nil {
		return fmt.Errorf("check admin count: %w", err)
	}

	if count > 0 {
		return nil // Admin already exists
	}

	username := os.Getenv("ADMIN_USERNAME")
	if username == "" {
		username = "admin"
	}

	password := os.Getenv("ADMIN_PASSWORD")
	if password == "" {
		password = "admin123"
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	_, err = db.Exec(
		"INSERT INTO admin_users (username, password_hash) VALUES ($1, $2)",
		username, string(hash),
	)
	if err != nil {
		return fmt.Errorf("seed admin: %w", err)
	}

	fmt.Printf("Seeded default admin user: %s\n", username)
	return nil
}

// --- JWT ---

type Claims struct {
	UserID   int    `json:"user_id"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

func getJWTSecret() []byte {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "otzi-jwt-secret-change-in-production"
	}
	return []byte(secret)
}

func generateToken(userID int, username string) (string, int64, error) {
	expiresIn := int64(24 * 60 * 60) // 24 hours
	now := time.Now()
	claims := Claims{
		UserID:   userID,
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        fmt.Sprintf("%d-%d", userID, now.UnixNano()), // unique JTI
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Duration(expiresIn) * time.Second)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "otzi-amunet",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signedToken, err := token.SignedString(getJWTSecret())
	if err != nil {
		return "", 0, err
	}

	return signedToken, expiresIn, nil
}

// cleanupExpiredBlacklist removes expired entries from the token blacklist.
func cleanupExpiredBlacklist() {
	_, err := db.Exec("DELETE FROM token_blacklist WHERE expires_at < NOW()")
	if err != nil {
		log.Printf("Failed to cleanup token blacklist: %v", err)
	}
}

// --- Middleware ---

// AuthMiddleware checks either:
//   1. Authorization: Bearer <jwt> (browser sessions)
//   2. X-Admin-API-Key header (machine-to-machine, e.g. processor)
// If neither is valid, returns 401.
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Try JWT first
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
			claims := &Claims{}
			token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
				return getJWTSecret(), nil
			})

			if err == nil && token.Valid {
				// Check if token has been blacklisted (logged out)
				var blacklisted int
				err := db.QueryRow(
					"SELECT COUNT(*) FROM token_blacklist WHERE token_jti = $1",
					claims.ID,
				).Scan(&blacklisted)

				if err == nil && blacklisted == 0 {
					c.Set("auth_user_id", claims.UserID)
					c.Set("auth_username", claims.Username)
					c.Set("auth_method", "jwt")
					c.Set("auth_token_jti", claims.ID)
					c.Next()
					return
				}
			}
		}

		// Fallback: X-Admin-API-Key (machine-to-machine)
		apiKey := c.GetHeader("X-Admin-API-Key")
		if apiKey != "" && apiKey == os.Getenv("ADMIN_API_KEY") {
			c.Set("auth_method", "api_key")
			c.Next()
			return
		}

		c.JSON(http.StatusUnauthorized, gin.H{
			"status":  "error",
			"message": "Unauthorized: valid JWT or API key required",
		})
		c.Abort()
	}
}

// --- Handlers ---

func handleLogin(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"message": "Username and password are required",
		})
		return
	}

	// Fetch user from DB
	var user AdminUser
	err := db.QueryRow(
		"SELECT id, username, password_hash FROM admin_users WHERE username = $1",
		req.Username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusUnauthorized, gin.H{
			"status":  "error",
			"message": "Invalid username or password",
		})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":  "error",
			"message": "Database error",
		})
		return
	}

	// Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"status":  "error",
			"message": "Invalid username or password",
		})
		return
	}

	// Generate token
	token, expiresIn, err := generateToken(user.ID, user.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":  "error",
			"message": "Failed to generate token",
		})
		return
	}

	c.JSON(http.StatusOK, LoginResponse{
		Token:     token,
		ExpiresIn: expiresIn,
		Username:  user.Username,
	})
}

func handleMe(c *gin.Context) {
	// Returns current user info based on auth context set by middleware
	username, _ := c.Get("auth_username")
	method, _ := c.Get("auth_method")

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"user": gin.H{
			"username": username,
		},
		"auth_method": method,
	})
}

func handleLogout(c *gin.Context) {
	// Clean up expired blacklist entries first (best-effort)
	cleanupExpiredBlacklist()

	// Get the JTI from the middleware context
	jti, exists := c.Get("auth_token_jti")
	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"message": "Not a JWT-authenticated session",
		})
		return
	}

	// Blacklist the token's JTI
	// Set expires_at to 24h from now (a safety margin past the token's expiry)
	_, err := db.Exec(
		"INSERT INTO token_blacklist (token_jti, expires_at) VALUES ($1, NOW() + INTERVAL '24 hours') ON CONFLICT (token_jti) DO NOTHING",
		jti,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":  "error",
			"message": "Failed to invalidate session",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"message": "Logged out successfully",
	})
}
