package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
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

			// append adds p to the end of the list and gives back the
			// updated list, which we store back in posts.
			posts = append(posts, p)
		}

		// The loop above stops on "no more rows" OR on an error partway
		// through (like a dropped connection). rows.Err() tells us which.
		if err := rows.Err(); err != nil {
			log.Printf("list posts rows failed: %v", err)
			writeError(w, http.StatusInternalServerError, "something went wrong")
			return
		}

		// Everything worked: send the list as JSON with status 200 (OK).
		writeJSON(w, http.StatusOK, posts)
	}
}

// getPost answers GET /posts/{id} with one full post.
// Same closure pattern as listPosts.
func getPost(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// r.PathValue("id") gets whatever filled the {id} placeholder, as
		// text. strconv.ParseInt turns it into a number:
		//   10 = base 10 (normal decimal numbers)
		//   64 = fit it into an int64
		// If the text isn't a number (like /posts/hello), err is not nil.
		// "||" means "or": we also reject 0 and negative numbers, since
		// database ids start at 1.
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			// 400 Bad Request: the visitor asked for something malformed.
			writeError(w, http.StatusBadRequest, "invalid post id")
			return
		}

		var p Post

		// QueryRow is for queries that return (at most) one row.
		// $1 is a placeholder that pgx fills in with the id we pass after
		// the SQL. NEVER paste values directly into SQL text: placeholders
		// keep the value separate from the query, which is what prevents
		// "SQL injection" attacks.
		//
		// The trailing dot on the "id)." line lets us continue with .Scan on
		// the next line. Go requires the dot at the END of the line when you
		// split a chain like this.
		err = pool.QueryRow(r.Context(), `
			SELECT id, title, category, to_char(published_on, 'YYYY-MM-DD'), body
			FROM posts
			WHERE id = $1`, id).
			Scan(&p.ID, &p.Title, &p.Category, &p.PublishedOn, &p.Body)

		// errors.Is checks whether err is (or wraps) the specific
		// "no rows found" error from pgx. That's not a server failure; it
		// just means no post has this id, so we answer 404 Not Found.
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "post not found")
			return
		}
		// Any OTHER error is a real problem (database down, etc.).
		if err != nil {
			log.Printf("get post query failed: %v", err)
			writeError(w, http.StatusInternalServerError, "something went wrong")
			return
		}

		writeJSON(w, http.StatusOK, p)
	}
}

// =============================================================================
// MIDDLEWARE — code that runs around every request
// =============================================================================

// withCORS wraps the router so every response gets CORS headers.
//
// WHY THIS EXISTS: browsers block a web page from reading responses from a
// different domain unless that domain explicitly allows it. Your frontend
// (e.g. yourblog.com) and API (e.g. api.yourblog.com, or localhost:8080
// while developing) count as different. These headers are the API saying
// "I allow that site to read my responses".
//
// "next http.Handler" is the thing being wrapped (our router). This pattern
// (a function that takes a handler and returns a new handler that does
// something extra, then calls the original) is called "middleware".
func withCORS(allowedOrigin string, next http.Handler) http.Handler {
	// http.HandlerFunc(...) turns a plain function into an http.Handler.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowedOrigin != "" {
			// w.Header().Set adds a header to the response.
			// Allow-Origin: which site may read responses.
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			// Allow-Methods: which request types that site may use.
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			// Vary: Origin tells caches the response depends on which site
			// asked, so a cached copy isn't wrongly reused for another site.
			w.Header().Set("Vary", "Origin")
		}

		// Before some requests, browsers send an OPTIONS "preflight" request
		// to ask permission. We answer with 204 (No Content, meaning "OK,
		// nothing else to say") and stop, since the headers above are the
		// whole answer.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// For every normal request, hand it on to the router.
		next.ServeHTTP(w, r)
	})
}

// =============================================================================
// HELPERS — small functions used by the handlers above
// =============================================================================

// writeJSON sends any data as a JSON response with the given status code.
//
// "data any" means data can be ANY type: a list of posts, one post, a map.
// "status int" is the HTTP status code, e.g. 200 OK, 404 Not Found.
// Names like http.StatusOK are just readable names for those numbers.
func writeJSON(w http.ResponseWriter, status int, data any) {
	// Tell the browser the body is JSON.
	w.Header().Set("Content-Type", "application/json")

	// Send the status code. Headers must be set BEFORE this line; once the
	// status is written, headers can no longer change.
	w.WriteHeader(status)

	// json.NewEncoder(w) creates a JSON writer that writes straight into the
	// response. Encode(data) converts data to JSON text and sends it,
	// using the struct tags to name the fields.
	if err := json.NewEncoder(w).Encode(data); err != nil {
		// The status was already sent, so we can't send an error response
		// now. The best we can do is log it.
		log.Printf("could not write response: %v", err)
	}
}

// writeError sends an error in a consistent JSON shape, like:
//
//	{"error":"post not found"}
//
// so your frontend can always look for an "error" field.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
