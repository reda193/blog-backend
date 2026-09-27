package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

type PostSummary struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	PublishedOn string `json:"published_on"`
}

type Post struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	PublishedOn string `json:"published_on"`
	Body        string `json:"body"`
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, using env variables")
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	allowedOrigin := os.Getenv("CORS_ALLOWED_ORIGIN")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)

	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("could not create database pool: %v", err)
	}

	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("could not connect to database: %v", err)
	}
	log.Println("connected to database")

	mux := http.NewServeMux()

	mux.HandleFunc("GET /posts", listPosts(pool))

	mux.HandleFunc("GET /posts/{id}", getPost(pool))

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           withCORS(allowedOrigin, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("listening on http://localhost:%s", port)

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func listPosts(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), `
			SELECT id, title, category, to_char(published_on, 'YYYY-MM-DD')
			FROM posts
			ORDER BY published_on DESC, id DESC`)
		if err != nil {
			log.Printf("list posts query failed: %v", err)
			writeError(w, http.StatusInternalServerError, "something went wrong")
			return
		}
		defer rows.Close()

		posts := []PostSummary{}

		for rows.Next() {
			var p PostSummary

			if err := rows.Scan(&p.ID, &p.Title, &p.Category, &p.PublishedOn); err != nil {
				log.Printf("list posts scan failed: %v", err)
				writeError(w, http.StatusInternalServerError, "something went wrong")
				return
			}

			posts = append(posts, p)
		}

		if err := rows.Err(); err != nil {
			log.Printf("list posts rows failed: %v", err)
			writeError(w, http.StatusInternalServerError, "something went wrong")
			return
		}

		writeJSON(w, http.StatusOK, posts)
	}
}

func getPost(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			writeError(w, http.StatusBadRequest, "invalid post id")
			return
		}

		var p Post

		err = pool.QueryRow(r.Context(), `
			SELECT id, title, category, to_char(published_on, 'YYYY-MM-DD'), body
			FROM posts
			WHERE id = $1`, id).
			Scan(&p.ID, &p.Title, &p.Category, &p.PublishedOn, &p.Body)

		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "post not found")
			return
		}
		if err != nil {
			log.Printf("get post query failed: %v", err)
			writeError(w, http.StatusInternalServerError, "something went wrong")
			return
		}

		writeJSON(w, http.StatusOK, p)
	}
}

func withCORS(allowedOrigins string, next http.Handler) http.Handler {
	// Turn "a,b" into a set of allowed addresses.
	allowed := map[string]bool{}
	for _, o := range strings.Split(allowedOrigins, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			allowed[o] = true
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only answer with the address that asked, if it's on the list.
		origin := r.Header.Get("Origin")
		if allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		}
		w.Header().Set("Vary", "Origin")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")

	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(data); err != nil {

		log.Printf("could not write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
