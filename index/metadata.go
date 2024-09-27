package index

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"log"
)

var metadata_fetch_tasks_chan chan []MetadataFetchTask

func EnqueueMetadataFetchTasks(tasks []MetadataFetchTask) {
	metadata_fetch_tasks_chan <- tasks
}

func StartMetadataFetchTaskWorker(ctx context.Context, pool *pgxpool.Pool) {
	metadata_fetch_tasks_chan = make(chan []MetadataFetchTask, 100)
	go createTasksInDb(ctx, pool)
}

func createTasksInDb(ctx context.Context, pool *pgxpool.Pool) {
	for {
		tasks := <-metadata_fetch_tasks_chan
		log.Println("Got tasks to create in db. Count of tasks: ", len(tasks))
		conn, err := pool.Acquire(ctx)
		if err != nil {
			log.Printf("Error acquiring connection to create fetch tasks: %v", err)
			metadata_fetch_tasks_chan <- tasks
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			log.Printf("Error beginning transaction to create fetch tasks: %v", err)
			metadata_fetch_tasks_chan <- tasks
		}
		tx_failed := false
		for _, task := range tasks {
			_, err := tx.Exec(ctx, "INSERT INTO fetch_metadata_tasks (address, type, status) VALUES ($1, $2, 'ready') ON CONFLICT DO NOTHING", task.Address, task.Type)
			if err != nil {
				log.Printf("Error inserting fetch task: %v", err)
				tx.Rollback(ctx)
				metadata_fetch_tasks_chan <- tasks
				tx_failed = true
				break
			}
		}
		if !tx_failed {
			err = tx.Commit(ctx)
			if err != nil {
				log.Printf("Error committing transaction to create fetch tasks: %v", err)
				metadata_fetch_tasks_chan <- tasks
			}
		}
	}
}
