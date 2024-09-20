package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/semaphore"
	"io"
	"log"
	"net/http"
	"time"
)

var gate *semaphore.Weighted
var client *http.Client

type FetchTask struct {
	Type    string
	Address string
}

type AddressMetadata struct {
	Address     *string
	Type        *string
	Name        *string
	Symbol      *string
	Description *string
	Image       *string
	Extra       map[string]interface{}
}

func (receiver AddressMetadata) hasAnyData() bool {
	return *receiver.Name != "" || *receiver.Description != "" || *receiver.Image != ""
}

func fetchTasks(ctx context.Context, pool *pgxpool.Pool) ([]FetchTask, error) {
	// Acquire a connection from the pool
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire connection: %v", err)
	}
	defer conn.Release()

	rows, err := conn.Query(ctx, "SELECT type, address FROM fetch_metadata_tasks WHERE status='not_started' LIMIT 100")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tasks: %v", err)
	}
	defer rows.Close()

	var tasks []FetchTask
	for rows.Next() {
		var task FetchTask
		if err := rows.Scan(&task.Type, &task.Address); err != nil {
			return nil, fmt.Errorf("failed to scan task: %v", err)
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func getMetadata(ctx context.Context, tx pgx.Tx, task FetchTask) (map[string]interface{}, error) {
	var metadataBytes []byte
	var fieldName string
	switch task.Type {
	case "nft_collections":
		fieldName = "collection_content"
	case "nft_items":
		fieldName = "content"
	case "jetton_masters":
		fieldName = "jetton_content"
	}
	query := fmt.Sprintf("SELECT %s as metadata FROM %s WHERE address = $1", fieldName, task.Type)
	err := tx.QueryRow(ctx, query, task.Address).Scan(&metadataBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch metadata: %v", err)
	}

	var metadata map[string]interface{}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return nil, fmt.Errorf("failed to unmarshal metadata: %v", err)
	}
	return metadata, nil
}

// extractURL extracts the 'url' or 'uri' from the metadata.
func extractURL(metadata map[string]interface{}) (string, error) {
	if url, ok := metadata["url"].(string); ok {
		return url, nil
	}
	if uri, ok := metadata["uri"].(string); ok {
		return uri, nil
	}
	return "", fmt.Errorf("no 'url' or 'uri' found in metadata")
}

// completeTask removes the task from the tasks table.
func completeTask(ctx context.Context, tx pgx.Tx, task FetchTask) error {
	query := "DELETE FROM fetch_metadata_tasks WHERE type = $1 AND address = $2"
	_, err := tx.Exec(ctx, query, task.Type, task.Address)
	if err != nil {
		return fmt.Errorf("failed to delete task: %v", err)
	}
	return nil
}

func getMetadataFromJson(metadata map[string]interface{}) AddressMetadata {
	var result AddressMetadata
	for key := range metadata {
		if value, ok := metadata[key].(string); ok {
			switch key {
			case "name":
				if result.Name == nil {
					result.Name = new(string)
				}
				*result.Name = value
			case "description":
				if result.Description == nil {
					result.Description = new(string)
				}
				*result.Description = value
			case "image":
				if result.Image == nil {
					result.Image = new(string)
				}
				*result.Image = value
			case "symbol":
				if result.Symbol == nil {
					result.Symbol = new(string)
				}
				*result.Symbol = value
			default:
				if result.Extra == nil {
					result.Extra = make(map[string]interface{})
				}
				result.Extra[key] = value
			}
		}
	}

	return result
}

func fetchContent(metadata map[string]interface{}) (AddressMetadata, error) {
	url, err := extractURL(metadata)
	if err != nil {
		metadataFromDb := getMetadataFromJson(metadata)
		if metadataFromDb.hasAnyData() {
			return metadataFromDb, nil
		} else {
			return AddressMetadata{}, fmt.Errorf("failed to extract URL or required data: %v", err)
		}
	}

	resp, err := client.Get(url)
	if err != nil {
		return AddressMetadata{}, fmt.Errorf("failed to fetch content from URL: %v", err)
	}
	defer resp.Body.Close()

	// check body is json
	if resp.Header.Get("Content-Type") != "application/json" {
		return AddressMetadata{}, fmt.Errorf("non-JSON content type: %s", resp.Header.Get("Content-Type"))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AddressMetadata{}, fmt.Errorf("non-OK HTTP status: %s", resp.Status)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return AddressMetadata{}, fmt.Errorf("failed to read response body: %v", err)
	}

	var content map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &content); err != nil {
		return AddressMetadata{}, fmt.Errorf("failed to unmarshal response body: %v", err)
	}
	return getMetadataFromJson(content), nil
}

func processTask(ctx context.Context, pool *pgxpool.Pool, task FetchTask) (taskError error) {
	defer gate.Release(1)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %v", err)
	}
	defer func() {
		if taskError != nil {
			_ = tx.Rollback(ctx)
		} else {
			_ = tx.Commit(ctx)
		}
	}()

	// Process the task within the transaction
	metadata, err := getMetadata(ctx, tx, task)
	if err != nil {
		log.Fatal(err)
		return err
	}

	content, err := fetchContent(metadata)
	if err != nil {
		_, err := tx.Exec(ctx, `INSERT INTO address_metadata (address, type, valid, name, description, image, symbol, extra) 
							VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			task.Address, task.Type, false, nil, nil, nil, nil, nil)
		if err != nil {
			log.Fatal(err)

			return err
		}
		if err := completeTask(ctx, tx, task); err != nil {
			log.Fatal(err)

			return err
		}
		return nil
	}

	_, err = tx.Exec(ctx, `INSERT INTO address_metadata (address, type, valid, name, description, image, symbol, extra)
    							VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (address, type) DO UPDATE SET 
    							valid = $3, name = $4, description = $5, image = $6, symbol = $7, extra = $8`,
		task.Address, task.Type, true, content.Name, content.Description, content.Image, content.Symbol, content.Extra)
	if err != nil {
		log.Fatal(err)

		return err
	}
	if err := completeTask(ctx, tx, task); err != nil {
		log.Fatal(err)

		return err
	}
	return nil
}

func initializeDb(ctx context.Context, pgDsn string, processes int) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(pgDsn)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %v", err)
	}
	// Set maximum connections in the pool
	config.MaxConns = max(int32(processes)*2, 4)

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %v", err)
	}
	return pool, nil
}

func updateStalledTasks(ctx context.Context, pool *pgxpool.Pool) {
	for {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			log.Fatal("failed to acquire connection: ", err)
		}

		_, err = conn.Exec(ctx, "UPDATE fetch_metadata_tasks SET status='not_started' WHERE status='in_progress' AND started_at < NOW() - INTERVAL '5 minutes'")
		if err != nil {
			log.Fatal("failed to update stalled tasks: ", err)
		}
		conn.Release()
		time.Sleep(time.Minute)
	}
}

func main() {
	var pg_dsn string
	var processes int
	flag.StringVar(&pg_dsn, "pg", "postgresql://localhost:5432", "PostgreSQL connection string")
	flag.IntVar(&processes, "processes", 32, "Set number of parallel queries")
	flag.Parse()

	gate = semaphore.NewWeighted(int64(processes))
	client = &http.Client{
		Timeout: 30 * time.Second,
	}
	ctx := context.Background()
	pool, err := initializeDb(ctx, pg_dsn, processes)

	if err != nil {
		log.Fatal("Error initializing database connection: ", err)
	}
	defer pool.Close()
	go updateStalledTasks(ctx, pool)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		log.Fatal("failed to acquire connection: ", err)
	}
	defer conn.Release()
	for {
		tasks, err := fetchTasks(ctx, pool)
		if err != nil {
			log.Println("Error fetching tasks: ", err)
			time.Sleep(time.Second)
			continue
		}

		for _, task := range tasks {
			_, err := conn.Query(ctx, "UPDATE fetch_metadata_tasks SET status='in_progress', started_at=NOW() WHERE type = $1 AND address = $2", task.Type, task.Address)
			if err != nil {
				continue
			}
			err = gate.Acquire(ctx, 1)
			if err != nil {
				log.Fatalf("failed to acquire worker: %s", err.Error())
			}
			go processTask(ctx, pool, task)
		}
		time.Sleep(time.Second)
	}

}
