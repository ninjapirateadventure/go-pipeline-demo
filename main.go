package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
)

// Configuration structure
type Config struct {
	Verbose              bool    `json:"verbose"`
	TotalItems          int     `json:"total_items"`
	DispatchSize        int     `json:"dispatch_size"`
	GeneralChannelSize  int     `json:"general_channel_size"`
	DatabaseChannelSize int     `json:"database_channel_size"`
	ForcePanicProb      float64 `json:"force_panic_probability"`
}

// Default configuration
func DefaultConfig() *Config {
	return &Config{
		Verbose:              true,
		TotalItems:          10,
		DispatchSize:        5,
		GeneralChannelSize:  10,
		DatabaseChannelSize: 20,
		ForcePanicProb:      0.01, // 1% chance of panic
	}
}

// State constants
const (
	StateProduced           = "produced"
	StateDispatched         = "dispatched"
	StateRepresentationProb = "representation_problem"
	StateSizeProblem        = "size_problem"
	StateTripled            = "tripled"
	StateReversed           = "reversed"
	StateInducedError       = "induced_error"
)

// Item struct for channels (minimal)
type Item struct {
	ID    string `json:"id"`
	Value string `json:"value"` // 6-digit string
}

// State transition for history
type StateTransition struct {
	State     string    `json:"state"`
	Value     string    `json:"value"`
	Timestamp time.Time `json:"timestamp"`
}

// Full item record for database
type ItemRecord struct {
	ID           string            `json:"id" firestore:"id"`
	CurrentState string            `json:"current_state" firestore:"current_state"`
	CurrentValue string            `json:"current_value" firestore:"current_value"`
	StateHistory []StateTransition `json:"state_history" firestore:"state_history"`
	CreatedAt    time.Time         `json:"created_at" firestore:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at" firestore:"updated_at"`
}

// Database interface
type DatabaseInterface interface {
	CreateItem(ctx context.Context, record *ItemRecord) error
	UpdateItem(ctx context.Context, id, state, value string) error
	GetItem(ctx context.Context, id string) (*ItemRecord, error)
	GetItemsByState(ctx context.Context, state string, limit int) ([]*ItemRecord, error)
	GetItemsByDateRange(ctx context.Context, start, end time.Time) ([]*ItemRecord, error)
}

// Pipeline represents our processing system
type Pipeline struct {
	config     *Config
	db         DatabaseInterface
	
	// Channels
	transitionCh     chan TransitionRequest
	checkRepCh       chan Item
	getEvenCh        chan Item
	tripleCh         chan Item
	tripleLoopCh     chan Item // Self-loop for triple retries
}

// Request structure for transition operations
type TransitionRequest struct {
	ID    string
	Value string
	State string
}

// Helper functions
func hasTripleSameDigit(value string) bool {
	// Ensure exactly 6 digits by padding if necessary
	padded := fmt.Sprintf("%06s", value)
	if len(padded) > 6 {
		padded = padded[len(padded)-6:]
	}
	
	digitCount := make(map[rune]int)
	for _, digit := range padded {
		digitCount[digit]++
		if digitCount[digit] >= 3 {
			return true
		}
	}
	return false
}

func removeOddDigits(value string) string {
	var result strings.Builder
	for _, char := range value {
		if char >= '0' && char <= '9' {
			digit := int(char - '0')
			if digit%2 == 0 { // Keep even digits
				result.WriteRune(char)
			}
		}
	}
	return result.String()
}

func reverseDigits(value string) string {
	runes := []rune(value)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

func multiplyByThree(value string) (string, error) {
	num, err := strconv.Atoi(value)
	if err != nil {
		return "", err
	}
	result := num * 3
	return strconv.Itoa(result), nil
}

func divideByTwo(value string) (string, error) {
	num, err := strconv.Atoi(value)
	if err != nil {
		return "", err
	}
	result := num / 2
	return strconv.Itoa(result), nil
}

// NewPipeline creates a new pipeline instance
func NewPipeline(config *Config, db DatabaseInterface) *Pipeline {
	return &Pipeline{
		config:           config,
		db:              db,
		transitionCh:    make(chan TransitionRequest, config.DatabaseChannelSize),
		checkRepCh:      make(chan Item, config.GeneralChannelSize),
		getEvenCh:       make(chan Item, config.GeneralChannelSize),
		tripleCh:        make(chan Item, config.GeneralChannelSize),
		tripleLoopCh:    make(chan Item, config.GeneralChannelSize),
	}
}

// Transition worker - handles all database operations with panic simulation
func (p *Pipeline) TransitionWorker(ctx context.Context) {
	for {
		select {
		case req, ok := <-p.transitionCh:
			if !ok {
				log.Printf("Transition channel closed, worker stopping")
				return
			}
			
			// Handle the request with panic recovery
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("Recovered from panic in TransitionWorker for item %s: %v", req.ID, r)
						// Send induced error state (avoid infinite recursion)
						select {
						case p.transitionCh <- TransitionRequest{
							ID:    req.ID,
							Value: req.Value,
							State: StateInducedError,
						}:
						default:
							log.Printf("Could not send induced error state for %s", req.ID)
						}
					}
				}()
				
				// Simulate panic before processing
				if rand.Float64() < p.config.ForcePanicProb {
					log.Printf("Simulated panic for item %s", req.ID)
					panic(fmt.Sprintf("Simulated panic for item %s", req.ID))
				}

				if p.config.Verbose {
					log.Printf("Transition: %s -> %s (value: %s)", req.ID, req.State, req.Value)
				}

				// Check if item exists
				_, err := p.db.GetItem(ctx, req.ID)
				if err != nil {
					// Item doesn't exist - create new item
					record := &ItemRecord{
						ID:           req.ID,
						CurrentState: req.State,
						CurrentValue: req.Value,
						StateHistory: []StateTransition{{
							State:     req.State,
							Value:     req.Value,
							Timestamp: time.Now(),
						}},
						CreatedAt: time.Now(),
						UpdatedAt: time.Now(),
					}
					if err := p.db.CreateItem(ctx, record); err != nil {
						log.Printf("Error creating item %s: %v", req.ID, err)
					}
				} else {
					// Item exists - update it
					if err := p.db.UpdateItem(ctx, req.ID, req.State, req.Value); err != nil {
						log.Printf("Error updating item %s: %v", req.ID, err)
					}
				}
			}()
			
		case <-ctx.Done():
			log.Printf("TransitionWorker stopped due to context cancellation")
			return
		}
	}
}

// Produce step - generates items
func (p *Pipeline) Produce(ctx context.Context) error {
	log.Printf("Producing %d items", p.config.TotalItems)
	
	for i := 0; i < p.config.TotalItems; i++ {
		id := uuid.New().String()
		value := fmt.Sprintf("%05d", rand.Intn(100000)) // 5-digit number as string
		
		// Send to transition for "produced" state
		select {
		case p.transitionCh <- TransitionRequest{ID: id, Value: value, State: StateProduced}:
		case <-ctx.Done():
			return ctx.Err()
		}
		
		if p.config.Verbose && (i+1)%100 == 0 {
			log.Printf("Produced %d items", i+1)
		}
	}
	
	log.Printf("Production completed: %d items", p.config.TotalItems)
	return nil
}

// Dispatch step - reads produced items and sends them through the pipeline
func (p *Pipeline) Dispatch(ctx context.Context) error {
	log.Printf("Starting dispatch with batch size %d", p.config.DispatchSize)
	
	for {
		// Get batch of produced items
		items, err := p.db.GetItemsByState(ctx, StateProduced, p.config.DispatchSize)
		if err != nil {
			return fmt.Errorf("error fetching produced items: %v", err)
		}
		
		if len(items) == 0 {
			log.Printf("No more produced items to dispatch")
			break
		}
		
		log.Printf("Dispatching batch of %d items", len(items))
		
		for _, item := range items {
			// Transition to dispatched
			select {
			case p.transitionCh <- TransitionRequest{ID: item.ID, Value: item.CurrentValue, State: StateDispatched}:
			case <-ctx.Done():
				return ctx.Err()
			}
			
			// Send to check representation
			select {
			case p.checkRepCh <- Item{ID: item.ID, Value: item.CurrentValue}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		
		// Small delay between batches
		time.Sleep(100 * time.Millisecond)
	}
	
	return nil
}

// CheckRepresentation worker
func (p *Pipeline) CheckRepresentationWorker(ctx context.Context) {
	defer close(p.getEvenCh)
	
	for item := range p.checkRepCh {
		if hasTripleSameDigit(item.Value) {
			// Send to transition with error state
			select {
			case p.transitionCh <- TransitionRequest{ID: item.ID, Value: item.Value, State: StateRepresentationProb}:
			case <-ctx.Done():
				return
			}
		} else {
			// Send to next stage
			select {
			case p.getEvenCh <- item:
			case <-ctx.Done():
				return
			}
		}
	}
}

// GetEven worker
func (p *Pipeline) GetEvenWorker(ctx context.Context) {
	defer close(p.tripleCh)
	
	for item := range p.getEvenCh {
		newValue := removeOddDigits(item.Value)
		
		if len(newValue) < 2 {
			// Send to transition with error state
			select {
			case p.transitionCh <- TransitionRequest{ID: item.ID, Value: newValue, State: StateSizeProblem}:
			case <-ctx.Done():
				return
			}
		} else {
			// Send to triple stage
			select {
			case p.tripleCh <- Item{ID: item.ID, Value: newValue}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// Triple worker with self-loop for retries
func (p *Pipeline) TripleWorker(ctx context.Context) {
	// Start the self-loop goroutine
	go func() {
		for item := range p.tripleLoopCh {
			select {
			case p.tripleCh <- item:
			case <-ctx.Done():
				return
			}
		}
	}()
	
	for item := range p.tripleCh {
		tripled, err := multiplyByThree(item.Value)
		if err != nil {
			log.Printf("Error tripling value for item %s: %v", item.ID, err)
			continue
		}
		
		if len(tripled) >= 5 {
			// Divide by 2 and retry via self-loop
			halved, err := divideByTwo(item.Value)
			if err != nil {
				log.Printf("Error halving value for item %s: %v", item.ID, err)
				continue
			}
			
			if p.config.Verbose {
				log.Printf("Triple retry: %s value %s -> %s", item.ID, item.Value, halved)
			}
			
			select {
			case p.tripleLoopCh <- Item{ID: item.ID, Value: halved}:
			case <-ctx.Done():
				return
			}
		} else {
			// Success - send to transition
			select {
			case p.transitionCh <- TransitionRequest{ID: item.ID, Value: tripled, State: StateTripled}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// Reverse step - reads tripled items and reverses them
func (p *Pipeline) Reverse(ctx context.Context) error {
	log.Printf("Starting reverse with batch size %d", p.config.DispatchSize)
	
	for {
		// Get batch of tripled items
		items, err := p.db.GetItemsByState(ctx, StateTripled, p.config.DispatchSize)
		if err != nil {
			return fmt.Errorf("error fetching tripled items: %v", err)
		}
		
		if len(items) == 0 {
			log.Printf("No more tripled items to reverse")
			break
		}
		
		log.Printf("Reversing batch of %d items", len(items))
		
		for _, item := range items {
			reversed := reverseDigits(item.CurrentValue)
			
			// Send to transition
			select {
			case p.transitionCh <- TransitionRequest{ID: item.ID, Value: reversed, State: StateReversed}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		
		// Small delay between batches
		time.Sleep(100 * time.Millisecond)
	}
	
	return nil
}

// StartWorkers starts all the processing workers
func (p *Pipeline) StartWorkers(ctx context.Context) {
	// Start transition worker
	go p.TransitionWorker(ctx)
	
	// Start processing workers
	go p.CheckRepresentationWorker(ctx)
	go p.GetEvenWorker(ctx)
	go p.TripleWorker(ctx)
}

// RunPipeline executes the full pipeline: Produce -> Dispatch -> Reverse
func (p *Pipeline) RunPipeline(ctx context.Context) error {
	log.Printf("Starting pipeline execution")
	
	// Start all workers
	p.StartWorkers(ctx)
	
	// Run the main steps in sequence
	if err := p.Produce(ctx); err != nil {
		return fmt.Errorf("produce failed: %v", err)
	}
	
	// Wait a bit for transitions to complete
	time.Sleep(2 * time.Second)
	
	if err := p.Dispatch(ctx); err != nil {
		return fmt.Errorf("dispatch failed: %v", err)
	}
	
	// Wait for processing pipeline to complete
	time.Sleep(5 * time.Second)
	
	if err := p.Reverse(ctx); err != nil {
		return fmt.Errorf("reverse failed: %v", err)
	}
	
	// Wait for final transitions
	time.Sleep(2 * time.Second)
	
	log.Printf("Pipeline execution completed")
	return nil
}

// Firestore database implementation
type FirestoreDB struct {
	client     *firestore.Client
	collection string
}

func NewFirestoreDB(ctx context.Context, projectID string) (*FirestoreDB, error) {
	client, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to create Firestore client: %v", err)
	}
	
	return &FirestoreDB{
		client:     client,
		collection: "pipeline_items",
	}, nil
}

func (f *FirestoreDB) Close() error {
	return f.client.Close()
}

func (f *FirestoreDB) CreateItem(ctx context.Context, record *ItemRecord) error {
	_, err := f.client.Collection(f.collection).Doc(record.ID).Set(ctx, record)
	return err
}

func (f *FirestoreDB) UpdateItem(ctx context.Context, id, state, value string) error {
	docRef := f.client.Collection(f.collection).Doc(id)
	
	// Get current document to append to history
	doc, err := docRef.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get document: %v", err)
	}
	
	var item ItemRecord
	if err := doc.DataTo(&item); err != nil {
		return fmt.Errorf("failed to parse document: %v", err)
	}
	
	// Update the item
	item.CurrentState = state
	item.CurrentValue = value
	item.UpdatedAt = time.Now()
	item.StateHistory = append(item.StateHistory, StateTransition{
		State:     state,
		Value:     value,
		Timestamp: time.Now(),
	})
	
	_, err = docRef.Set(ctx, &item)
	return err
}

func (f *FirestoreDB) GetItem(ctx context.Context, id string) (*ItemRecord, error) {
	doc, err := f.client.Collection(f.collection).Doc(id).Get(ctx)
	if err != nil {
		return nil, err
	}
	
	var item ItemRecord
	if err := doc.DataTo(&item); err != nil {
		return nil, err
	}
	
	return &item, nil
}

func (f *FirestoreDB) GetItemsByState(ctx context.Context, state string, limit int) ([]*ItemRecord, error) {
	query := f.client.Collection(f.collection).
		Where("current_state", "==", state).
		Limit(limit)
	
	iter := query.Documents(ctx)
	defer iter.Stop()
	
	var items []*ItemRecord
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		
		var item ItemRecord
		if err := doc.DataTo(&item); err != nil {
			log.Printf("Error parsing document %s: %v", doc.Ref.ID, err)
			continue
		}
		
		items = append(items, &item)
	}
	
	return items, nil
}

func (f *FirestoreDB) GetItemsByDateRange(ctx context.Context, start, end time.Time) ([]*ItemRecord, error) {
	query := f.client.Collection(f.collection).
		Where("created_at", ">=", start).
		Where("created_at", "<=", end)
	
	iter := query.Documents(ctx)
	defer iter.Stop()
	
	var items []*ItemRecord
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		
		var item ItemRecord
		if err := doc.DataTo(&item); err != nil {
			log.Printf("Error parsing document %s: %v", doc.Ref.ID, err)
			continue
		}
		
		items = append(items, &item)
	}
	
	return items, nil
}

// HTTP handlers
func (p *Pipeline) handleRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	if err := p.RunPipeline(ctx); err != nil {
		http.Error(w, fmt.Sprintf("Pipeline failed: %v", err), http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "completed"})
}

func (p *Pipeline) handleItemReport(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}
	
	item, err := p.db.GetItem(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf("Item not found: %v", err), http.StatusNotFound)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(item)
}

func (p *Pipeline) handleStats(w http.ResponseWriter, r *http.Request) {
	// Get all items and count states
	ctx := r.Context()
	
	// Query all items (in production, you'd want pagination)
	query := p.db.(*FirestoreDB).client.Collection(p.db.(*FirestoreDB).collection)
	iter := query.Documents(ctx)
	defer iter.Stop()
	
	stateCounts := make(map[string]int)
	totalItems := 0
	
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("Error querying items: %v", err), http.StatusInternalServerError)
			return
		}
		
		var item ItemRecord
		if err := doc.DataTo(&item); err != nil {
			log.Printf("Error parsing document: %v", err)
			continue
		}
		
		stateCounts[item.CurrentState]++
		totalItems++
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_items":  totalItems,
		"state_counts": stateCounts,
	})
}

// Main function
func main() {
	rand.Seed(time.Now().UnixNano())
	
	// Get project ID from environment or gcloud config
	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		log.Fatal("GOOGLE_CLOUD_PROJECT environment variable must be set")
	}
	
	// Create configuration
	config := DefaultConfig()
	
	// Create Firestore database connection
	ctx := context.Background()
	db, err := NewFirestoreDB(ctx, projectID)
	if err != nil {
		log.Fatalf("Failed to create Firestore client: %v", err)
	}
	defer db.Close()
	
	log.Printf("Connected to Firestore in project: %s", projectID)
	
	// Create pipeline
	pipeline := NewPipeline(config, db)
	
	// Set up HTTP handlers
	http.HandleFunc("/run", pipeline.handleRun)
	http.HandleFunc("/item", pipeline.handleItemReport)
	http.HandleFunc("/stats", pipeline.handleStats)
	
	log.Printf("Server starting on :8080")
	log.Printf("Endpoints:")
	log.Printf("  POST /run - Run the full pipeline")
	log.Printf("  GET /item?id=<uuid> - Get item details")
	log.Printf("  GET /stats - Get state statistics")
	
	log.Fatal(http.ListenAndServe(":8080", nil))
}
