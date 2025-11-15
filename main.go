package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- CONFIGURATION ---
const (
	// ONLY CHECK PAGE 1 - Where all new listings appear
	API_URL_PAGE_1 = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsFilters?PageNo=1&PageSize=100&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&MasterBrand=113&sorting=&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="
	
	TELEGRAM_BOT_TOKEN = "8222224289:AAFDgJ2C0KSTks9lLhPKtUtR1KzqraNkybI"
	TELEGRAM_CHAT_ID   = "-4985438208"
	ADMIN_CHAT_ID      = "837428747"
	SEEN_ITEMS_FILE    = "seen_hotwheels_go.txt"
)

// --- SHARED STATE & DATA STRUCTS ---
var (
	mutex          sync.Mutex
	checkInterval  = 5 * time.Second // FAST 5 SECOND CHECKS
	isPaused       = false
	heartbeatMuted = true // Muted by default for fast checks
	seenItems      = make(map[string]bool)
	checkHistory   []CheckResult
	
	// Performance: reuse a single http client with optimized settings
	httpClient = &http.Client{
		Timeout: 8 * time.Second, // Shorter timeout for faster failures
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	
	// Writer channel for batch disk writes
	seenWriterCh chan string
	writerWg     sync.WaitGroup
)

// --- TELEGRAM STRUCTS ---
type TelegramUpdateResponse struct {
	Ok     bool     `json:"ok"`
	Result []Update `json:"result"`
}
type Update struct {
	UpdateID int     `json:"update_id"`
	Message  Message `json:"message"`
}
type Message struct {
	Text string `json:"text"`
	Chat Chat   `json:"chat"`
}
type Chat struct {
	ID int64 `json:"id"`
}

// --- API STRUCTS ---
type OuterEnvelope struct {
	ProductResponse string `json:"ProductResponse"`
}
type InnerData struct {
	Products []Product `json:"Products"`
}
type Product struct {
	ProductID     string `json:"PId"`
	ProductInfoID string `json:"PInfId"`
	ProductName   string `json:"PNm"`
	Price         string `json:"discprice"`
	StockStatus   string `json:"CrntStock"`
}

// --- HISTORY STRUCT ---
type CheckResult struct {
	Timestamp     time.Time
	FoundProducts []Product
}

// --- HELPER FUNCTIONS ---
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9 ]+`)
var spaceRegex = regexp.MustCompile(`\s+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlphanumericRegex.ReplaceAllString(s, "")
	s = spaceRegex.ReplaceAllString(s, "-")
	return s
}

func constructFullURL(p Product) string {
	productSlug := slugify(p.ProductName)
	return fmt.Sprintf("https://www.firstcry.com/hot-wheels/%s/%s/product-detail", productSlug, p.ProductID)
}

func sendTelegramMessage(chatID, message string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TELEGRAM_BOT_TOKEN)
	payload := url.Values{}
	payload.Set("chat_id", chatID)
	payload.Set("text", message)
	payload.Set("parse_mode", "HTML")
	
	// Non-blocking send with timeout
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		_, err := client.PostForm(apiURL, payload)
		if err != nil {
			log.Printf("❌ Failed to send Telegram message: %v", err)
		}
	}()
}

func loadSeenItems() {
	f, err := os.Open(SEEN_ITEMS_FILE)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("Error opening seen items file: %v", err)
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line != "" {
			seenItems[line] = true
		}
	}
	if err := s.Err(); err != nil {
		log.Printf("Scanner error: %v", err)
	}
}

// Writer goroutine for async disk writes
func startSeenWriter(ch <-chan string) {
	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		f, err := os.OpenFile(SEEN_ITEMS_FILE, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf("Error opening file for writing seen items: %v", err)
			return
		}
		defer f.Close()
		w := bufio.NewWriter(f)
		for id := range ch {
			if _, err := w.WriteString(id + "\n"); err != nil {
				log.Printf("Error writing seen item: %v", err)
				continue
			}
			w.Flush()
		}
		w.Flush()
	}()
}

func saveNewItem(productInfoID string) {
	select {
	case seenWriterCh <- productInfoID:
	default:
		// Fallback direct write
		f, err := os.OpenFile(SEEN_ITEMS_FILE, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf("Error opening file for writing: %v", err)
			return
		}
		if _, err := f.WriteString(productInfoID + "\n"); err != nil {
			log.Printf("Error writing to file fallback: %v", err)
		}
		f.Close()
	}
}

func startKeepAlive() {
	appURL := "https://hh2-uaol.onrender.com"
	go func() {
		log.Println("⏰ Keep-alive will start in 2 minutes...")
		time.Sleep(2 * time.Minute)

		ticker := time.NewTicker(8 * time.Minute)
		defer ticker.Stop()

		log.Printf("🔄 Keep-alive service started, pinging: %s", appURL)
		client := &http.Client{Timeout: 30 * time.Second}

		resp, err := client.Get(appURL + "/ping")
		if err != nil {
			log.Printf("⚠️ Initial keep-alive ping failed: %v", err)
		} else {
			resp.Body.Close()
			log.Printf("✅ Initial keep-alive ping successful (status: %d)", resp.StatusCode)
		}

		for range ticker.C {
			resp, err := client.Get(appURL + "/ping")
			if err != nil {
				log.Printf("⚠️ Keep-alive ping failed: %v", err)
			} else {
				resp.Body.Close()
				log.Printf("✅ Keep-alive ping successful (status: %d)", resp.StatusCode)
			}
		}
	}()
}

// --- OPTIMIZED SINGLE PAGE API FETCH ---
func fetchPage1Products(ctx context.Context) ([]Product, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", API_URL_PAGE_1, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", "https://www.firstcry.com/")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("bad status code: %d", resp.StatusCode)
	}
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	
	var outer OuterEnvelope
	if err := json.Unmarshal(body, &outer); err != nil {
		var altOuter map[string]interface{}
		if err2 := json.Unmarshal(body, &altOuter); err2 == nil {
			if respStr, ok := altOuter["ProductResponse"].(string); ok {
				outer.ProductResponse = respStr
			} else {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	
	if outer.ProductResponse == "" {
		return []Product{}, nil
	}
	
	var inner InnerData
	if err := json.Unmarshal([]byte(outer.ProductResponse), &inner); err != nil {
		return nil, err
	}
	
	return inner.Products, nil
}

// --- CORE LOGIC ---
func initializeBaseline() {
	log.Println("No baseline file found. Performing initial scan of Page 1...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	products, err := fetchPage1Products(ctx)
	if err != nil {
		log.Printf("❌ Fatal error during baseline creation: %v", err)
		return
	}
	
	var initialItems []string
	for _, p := range products {
		if p.StockStatus != "0" {
			initialItems = append(initialItems, p.ProductInfoID)
		}
	}
	
	content := strings.Join(initialItems, "\n")
	os.WriteFile(SEEN_ITEMS_FILE, []byte(content), 0644)
	log.Printf("✅ Baseline created with %d IN-STOCK items from Page 1.", len(initialItems))
}

func checkForNewItems() []Product {
	checkStart := time.Now()
	var newProductsFound []Product
	
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	
	allProducts, err := fetchPage1Products(ctx)
	if err != nil {
		log.Printf("❌ Error fetching Page 1: %v", err)
		sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("⚠️ Bot encountered an error: %v", err))
		return newProductsFound
	}

	for _, p := range allProducts {
		if p.StockStatus == "0" {
			continue
		}

		uniqueID := p.ProductInfoID

		mutex.Lock()
		seen := seenItems[uniqueID]
		mutex.Unlock()

		if !seen {
			elapsed := time.Since(checkStart)
			log.Printf("🚨 NEW ITEM FOUND in %.2fs: %s", elapsed.Seconds(), p.ProductName)
			newProductsFound = append(newProductsFound, p)

			fullURL := constructFullURL(p)
			message := fmt.Sprintf(
				"<b>🔥 NEW HOT WHEELS!</b>\n\n<b>%s</b>\n<b>Price:</b> ₹%s\n\n<a href='%s'>🛒 BUY NOW</a>\n\n⚡ Found in %.1fs",
				p.ProductName, p.Price, fullURL, elapsed.Seconds(),
			)
			sendTelegramMessage(TELEGRAM_CHAT_ID, message)

			saveNewItem(uniqueID)
			mutex.Lock()
			seenItems[uniqueID] = true
			mutex.Unlock()
		}
	}
	
	return newProductsFound
}

func scraperWorker(stop chan struct{}) {
	log.Println("🔥 Starting FAST scraper (5s interval, Page 1 only)...")
	
	initialFinds := checkForNewItems()
	mutex.Lock()
	checkHistory = append(checkHistory, CheckResult{Timestamp: time.Now(), FoundProducts: initialFinds})
	mutex.Unlock()
	
	if len(initialFinds) == 0 {
		log.Println("...No new items on initial check.")
	}
	
	ticker := time.NewTicker(5 * time.Second) // Fixed 5-second ticker
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			mutex.Lock()
			paused := isPaused
			mutex.Unlock()
			
			if !paused {
				newlyFoundProducts := checkForNewItems()
				mutex.Lock()
				checkHistory = append(checkHistory, CheckResult{Timestamp: time.Now(), FoundProducts: newlyFoundProducts})
				if len(checkHistory) > 20 {
					checkHistory = checkHistory[1:]
				}
				mutex.Unlock()
				
				if len(newlyFoundProducts) == 0 {
					log.Println("✓ Check complete - no new items")
				}
			}
		case <-stop:
			log.Println("Scraper worker shutting down.")
			return
		}
	}
}

func commandListenerWorker(stop chan struct{}) {
	log.Println("🤖 Command listener started.")
	var lastUpdateID int
	
	for {
		apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", TELEGRAM_BOT_TOKEN, lastUpdateID+1)
		resp, err := http.Get(apiURL)
		if err != nil {
			log.Printf("Error getting updates: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		
		var updates TelegramUpdateResponse
		json.Unmarshal(body, &updates)
		
		for _, update := range updates.Result {
			lastUpdateID = update.UpdateID
			if update.Message.Text == "" || update.Message.Chat.ID == 0 {
				continue
			}
			
			chatIDStr := strconv.FormatInt(update.Message.Chat.ID, 10)
			if chatIDStr != ADMIN_CHAT_ID {
				sendTelegramMessage(chatIDStr, "Sorry, you are not authorized.")
				continue
			}
			
			parts := strings.Fields(update.Message.Text)
			command := parts[0]
			
			switch command {
			case "/start":
				mutex.Lock()
				isPaused = false
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "▶️ Bot resumed (5s checks).")
				
			case "/pause":
				mutex.Lock()
				isPaused = true
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "⏸️ Bot paused.")
				
			case "/stop":
				sendTelegramMessage(ADMIN_CHAT_ID, "🛑 Stopping bot...")
				close(stop)
				return
				
			case "/mute":
				mutex.Lock()
				heartbeatMuted = true
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "🔕 Heartbeat muted.")
				
			case "/unmute":
				mutex.Lock()
				heartbeatMuted = false
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "🔔 Heartbeat enabled.")
				
			case "/status":
				mutex.Lock()
				status := "▶️ Running"
				if isPaused {
					status = "⏸️ Paused"
				}
				itemCount := len(seenItems)
				mutex.Unlock()
				
				sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf(
					"<b>⚡ FAST MODE STATUS</b>\n\n%s\nCheck Interval: 5 seconds\nPage 1 Only: ✅\nTracked Items: %d\n\n🎯 Optimized for instant notifications!",
					status, itemCount,
				))
				
			case "/recent":
				var sb strings.Builder
				sb.WriteString("<b>🔎 Recent Finds (Last 20 Checks)</b>\n\n")
				
				mutex.Lock()
				totalFound := 0
				for i := len(checkHistory) - 1; i >= 0; i-- {
					result := checkHistory[i]
					if len(result.FoundProducts) > 0 {
						totalFound += len(result.FoundProducts)
						loc, _ := time.LoadLocation("Asia/Kolkata")
						sb.WriteString(fmt.Sprintf("<b>Found at %s:</b>\n", result.Timestamp.In(loc).Format("03:04:05 PM")))
						for _, p := range result.FoundProducts {
							fullURL := constructFullURL(p)
							sb.WriteString(fmt.Sprintf("• <a href='%s'>%s</a> - ₹%s\n", fullURL, p.ProductName, p.Price))
						}
						sb.WriteString("\n")
					}
				}
				mutex.Unlock()
				
				if totalFound == 0 {
					sb.WriteString("No new products found in recent checks.")
				}
				sendTelegramMessage(ADMIN_CHAT_ID, sb.String())
				
			case "/cleanup":
				sendTelegramMessage(ADMIN_CHAT_ID, "🧹 Starting cleanup verification...")
				
				// Fetch current products from Page 1
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				currentProducts, err := fetchPage1Products(ctx)
				cancel()
				
				if err != nil {
					sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("❌ Cleanup failed: %v", err))
					continue
				}
				
				// Build map of currently active product IDs
				activeIDs := make(map[string]bool)
				for _, p := range currentProducts {
					if p.StockStatus != "0" {
						activeIDs[p.ProductInfoID] = true
					}
				}
				
				mutex.Lock()
				beforeCount := len(seenItems)
				
				// Remove IDs that are no longer in stock/available
				for id := range seenItems {
					if !activeIDs[id] {
						delete(seenItems, id)
					}
				}
				
				afterCount := len(seenItems)
				removed := beforeCount - afterCount
				
				// Rewrite the file with only active items
				var activeItems []string
				for id := range seenItems {
					activeItems = append(activeItems, id)
				}
				mutex.Unlock()
				
				content := strings.Join(activeItems, "\n")
				err = os.WriteFile(SEEN_ITEMS_FILE, []byte(content), 0644)
				
				if err != nil {
					sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("❌ Failed to write cleaned file: %v", err))
				} else {
					sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf(
						"✅ <b>Cleanup Complete!</b>\n\n"+
							"Before: %d items\n"+
							"After: %d items\n"+
							"Removed: %d inactive items\n\n"+
							"File optimized and ready!",
						beforeCount, afterCount, removed,
					))
				}
				
			case "/reset":
				sendTelegramMessage(ADMIN_CHAT_ID, "⚠️ Are you sure? Reply with /confirmreset to reset the entire baseline.")
				
			case "/confirmreset":
				mutex.Lock()
				seenItems = make(map[string]bool)
				mutex.Unlock()
				
				// Delete the file
				os.Remove(SEEN_ITEMS_FILE)
				
				// Rebuild baseline
				initializeBaseline()
				loadSeenItems()
				
				sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf(
					"🔄 <b>Baseline Reset Complete!</b>\n\nNew baseline created with %d items from Page 1.",
					len(seenItems),
				))
			}
		}
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("--- 🔥 Hot Wheels Hunter FAST MODE (5s/Page 1) 🔥 ---")

	// HTTP server for Render
	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}

		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("🔥 Hot Wheels Hunter FAST MODE - 5 Second Checks! 🔥"))
		})

		http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			mutex.Lock()
			status := "running"
			if isPaused {
				status = "paused"
			}
			itemCount := len(seenItems)
			mutex.Unlock()

			response := fmt.Sprintf(`{
				"status": "%s",
				"check_interval_seconds": 5,
				"mode": "fast_page1_only",
				"tracked_items": %d,
				"bot": "Hot Wheels Hunter FAST",
				"timestamp": "%s"
			}`, status, itemCount, time.Now().Format("2006-01-02 15:04:05"))

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(response))
		})

		http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("pong"))
		})

		log.Printf("🌐 HTTP server starting on port %s", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Printf("❌ HTTP server error: %v", err)
		}
	}()

	startKeepAlive()

	seenWriterCh = make(chan string, 100)
	startSeenWriter(seenWriterCh)

	if _, err := os.Stat(SEEN_ITEMS_FILE); os.IsNotExist(err) {
		initializeBaseline()
	}
	
	loadSeenItems()
	log.Printf("✅ Loaded baseline with %d items.", len(seenItems))
	log.Println("⚡ FAST MODE: Checking Page 1 every 5 seconds for instant notifications!")
	
	stop := make(chan struct{})
	go scraperWorker(stop)
	go commandListenerWorker(stop)
	
	sendTelegramMessage(ADMIN_CHAT_ID, "🚀 Bot FAST MODE Online!\n\n⚡ 5-second checks\n📄 Page 1 only\n🎯 Instant notifications")

	<-stop
	close(seenWriterCh)
	writerWg.Wait()
	log.Println("--- Bot has been shut down. ---")
}
