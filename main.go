package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
)

type ShortUrl struct {
	Slug   string
	Target string
	Clicks int
	Ttl    time.Duration
}

type ServerSummary struct {
	KnownSlugs   []ShortUrl
	KeyspaceInfo string
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

var default_ttl, _ = time.ParseDuration("1h")

const runes = "abcdefghjklmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ1234567890"

func randomSlug() string {
	// alphanum, but without ioIO
	const length = 8
	b := make([]byte, length)
	for i := range b {
		b[i] = runes[rand.Intn(len(runes))]
	}
	return string(b)
}

func slugIsValid(slug string) bool {
	for _, char := range slug {
		if !strings.Contains(runes, string(char)) {
			return false
		}
	}
	return true
}

func slugFromKey(key string) (string, error) {
	// url:1234 -> 1234
	z := strings.SplitN(key, ":", 2)
	if len(z) == 2 {
		return z[1], nil
	}
	return "", errors.New("Cannot parse key")
}

func keyOfSlug(slug string) string {
	return "url:" + slug
}

func keyOfSlugHitCount(slug string) string {
	return "urlhitcount:" + slug
}

func store(redis_db redis.Client, ctx context.Context, target string) (ShortUrl, error) {
	// Persist a new short->long pair into the database, with 0 stats

	for attempt := 0; attempt < 10; attempt++ {
		slug := randomSlug()
		val, err := redis_db.SetNX(ctx, keyOfSlug(slug), target, default_ttl).Result()

		if err == nil && val == true {
			// Success
			log.Println("Successfully created new value", slug, "for target", target)

			new_short_url := ShortUrl{
				Slug:   slug,
				Target: target,
				Clicks: 0,
				Ttl:    default_ttl,
			}
			return new_short_url, nil
		} else {
			log.Println("Collision creating slug?", slug)
		}
	}

	return ShortUrl{}, errors.New("Could not store new url after several attempts")
}

func getDetailsOfKey(redis_db redis.Client, ctx context.Context, slug string) (ShortUrl, error) {
	var target *redis.StringCmd
	var counter *redis.IntCmd
	var ttl *redis.DurationCmd

	_, err := redis_db.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		target = pipe.Get(ctx, keyOfSlug(slug))
		counter = pipe.IncrBy(ctx, keyOfSlugHitCount(slug), 0)
		ttl = pipe.TTL(ctx, keyOfSlug(slug))
		return nil
	})

	if err == nil {
		return ShortUrl{
			Slug:   slug,
			Target: target.Val(),
			Clicks: int(counter.Val()),
			Ttl:    ttl.Val(),
		}, nil
	}
	return ShortUrl{}, err
}

func sampleExisting(redis_db redis.Client, ctx context.Context) []ShortUrl {

	r := []ShortUrl{}

	// Get a slice of keys, discard cursor
	if keys, _, err := redis_db.Scan(ctx, 0, keyOfSlug("*"), 10).Result(); err == nil {
		for _, v := range keys {
			if slug, err := slugFromKey(v); err == nil {
				if su, err := getDetailsOfKey(redis_db, ctx, slug); err == nil {
					r = append(r, su)
				}
			}
		}
	}
	return r
}

func main() {

	redis_db := redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "", // no password set
		DB:       0,  // use default DB
	})

	router := mux.NewRouter()

	router.HandleFunc("/{slug:[0-9A-Za-z]+}", func(w http.ResponseWriter, req *http.Request) {
		// Find the matching key in redis
		_, details := req.URL.Query()["details"]

		vars := mux.Vars(req)
		slug := vars["slug"]
		if !slugIsValid(slug) {
			w.WriteHeader(http.StatusNotAcceptable)
			fmt.Fprintf(w, "Invalid slug")
			return
		}
		if target, err := redis_db.Get(req.Context(), keyOfSlug(slug)).Result(); err == nil {
			var counter *redis.IntCmd
			if details {

				var ttl *redis.DurationCmd
				redis_db.Pipelined(req.Context(), func(pipe redis.Pipeliner) error {
					counter = pipe.IncrBy(req.Context(), keyOfSlugHitCount(slug), 0)
					ttl = pipe.TTL(req.Context(), keyOfSlug(slug))
					return nil
				})

				d := ShortUrl{
					Slug:   slug,
					Target: target,
					Clicks: int(counter.Val()),
					Ttl:    ttl.Val(),
				}
				t, _ := template.ParseFiles("details.html")
				t.Execute(w, d)
			} else {
				// Count the hit and extend the TTL

				redis_db.Pipelined(req.Context(), func(pipe redis.Pipeliner) error {
					counter = redis_db.Incr(req.Context(), keyOfSlugHitCount(slug))
					pipe.Expire(req.Context(), keyOfSlugHitCount(slug), default_ttl)
					pipe.Expire(req.Context(), keyOfSlug(slug), default_ttl)
					return nil
				})

				//counter, _ := redis_db.Incr(req.Context(), keyOfSlugHitCount(slug)).Result()
				log.Println("Incremented counter for slug", slug, "to", counter.Val())
				// do the redirect
				http.Redirect(w, req, target, http.StatusFound)
			}
			//fmt.Fprintf(w, target)

			return
			// Do the redirect
		}

		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "Slug uot found")

	})

	router.HandleFunc("/_create", func(w http.ResponseWriter, req *http.Request) {
		target := req.FormValue("target")

		if su, err := store(*redis_db, req.Context(), target); err == nil {
			// Success, redirect to info url
			http.Redirect(w, req, "/"+su.Slug+"?details", http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, "Failed to create: %v", err)
		}

	})

	router.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {

		summary := ServerSummary{}

		summary.KnownSlugs = sampleExisting(*redis_db, req.Context())

		if keyspace_stats, err := redis_db.Info(req.Context(), "keyspace").Result(); err == nil {
			summary.KeyspaceInfo = keyspace_stats
		}

		t, _ := template.ParseFiles("index.html")
		t.Execute(w, summary)

	})

	log.Println("Listing for requests at http://localhost:8000/")
	log.Fatal(http.ListenAndServe(":8000", handlers.CombinedLoggingHandler(os.Stdout, router)))
}
