// hotwheels_bot_render.go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// -----------------------
// CONFIGURATION
// -----------------------
const (
	API_URL_PAGE_1         = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsFilters?PageNo=1&PageSize=100&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&MasterBrand=113&sorting=&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="
	API_URL_PAGING_TEMPLATE = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsPaging?PageNo=%d&PageSize=20&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&sorting=&MasterBrand=113&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="
	PAGES_TO_SCAN          = 6
	SEEN_ITEMS_FILE        = "seen_hotwheels_go.txt"

	// Your existing Telegram settings (kept)
	TELEGRAM_BOT_TOKEN = "8222224289:AAFDgJ2C0KSTks9lLhPKtUtR1KzqraNkybI"
	TELEGRAM_CHAT_ID   = "-4985438208"
	ADMIN_CHAT_ID      = "837428747"
)

// -----------------------
// SHARED STATE & STRUCTS
// -----------------------
var (
	// concurrency-safe seen items using a plain map + mutex (fast enough, simple)
	seenItems   = make(map[string]bool) // key: ProductInfoID
	seenItemsMu sync.RWMutex

	mutex          sync.Mutex
	checkInterval  = 5 * time.Second // default 5s (user requested fast checks)
	isPaused       = false
	heartbeatMuted = false
	checkHistory   []CheckResult
)

// Telegram polling structs
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

// API structs
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

// History struct
type CheckResult struct {
	Timestamp     time.Time
	FoundProducts []Product
}

// -----------------------
// HELPERS
// -----------------------
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
	_, err := http.PostForm(apiURL, payload)
	if err != nil {
		log.Printf("❌ Failed to send Telegram message: %v", err)
	}
}

// -----------------------
// PERSISTENCE
// -----------------------
func loadSeenItems() {
	data, err := os.ReadFile(SEEN_ITEMS_FILE)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Error reading seen file: %v", err)
		}
		return
	}
	lines := strings.Split(string(data), "\n")
	seenItemsMu.Lock()
	for _, line := range lines {
		if line != "" {
			seenItems[line] = true
		}
	}
	seenItemsMu.Unlock()
}

func saveNewItem(productInfoID string) {
	// Append to disk and in-memory map
	f, err := os.OpenFile(SEEN_ITEMS_FILE, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Error opening file for writing: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(productInfoID + "\n"); err != nil {
		log.Printf("Error writing to file: %v", err)
		return
	}
	seenItemsMu.Lock()
	seenItems[productInfoID] = true
	seenItemsMu.Unlock()
}

// -----------------------
// GLOBAL HTTP CLIENT (Keep-Alive connections)
// -----------------------
var httpClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// -----------------------
// KEEP-ALIVE FOR RENDER
// -----------------------
const RENDER_PING_URL = "https://hh2-uaol.onrender.com" // <- your provided render URL

// startKeepAlive will ping your Render URL periodically to reduce spin-down.
// Warm-up: wait 2 minutes, then ping every 8 minutes (safe cadence).
func startKeepAlive() {
	go func() {
		log.Println("⏰ Keep-alive goroutine starting: waiting 2 minutes before first ping...")
		time.Sleep(2 * time.Minute)

		ticker := time.NewTicker(8 * time.Minute)
		defer ticker.Stop()

		client := &http.Client{Timeout: 20 * time.Second}

		// Initial ping (immediate after warm-up)
		resp, err := client.Get(RENDER_PING_URL + "/ping")
		if err != nil {
			log.Printf("⚠️ Initial keep-alive ping failed: %v", err)
		} else {
			resp.Body.Close()
			log.Printf("✅ Initial keep-alive ping successful (status: %d)", resp.StatusCode)
		}

		for {
			select {
			case <-ticker.C:
				resp, err := client.Get(RENDER_PING_URL + "/ping")
				if err != nil {
					log.Printf("⚠️ Keep-alive ping failed: %v", err)
					// do not crash; we'll try again on next tick
				} else {
					resp.Body.Close()
					log.Printf("✅ Keep-alive ping successful (status: %d)", resp.StatusCode)
				}
			}
		}
	}()
}

// -----------------------
// CORE API LOGIC
// -----------------------
func fetchAndParseAPI(apiURL string) ([]Product, error) {
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; HotWheelsBot/1.0)")
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
		// Attempt a fallback parse if the response shape is slightly different
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

// getAllProductsFromAPI fetches page1 then pages 2..PAGES_TO_SCAN concurrently
func getAllProductsFromAPI() ([]Product, error) {
	var allProducts []Product
	seenPInfIDs := make(map[string]bool)

	// PAGE 1 (special)
	page1Products, err := fetchAndParseAPI(API_URL_PAGE_1)
	if err != nil {
		log.Printf("❌ Failed to fetch Page 1: %v", err)
		// continue to attempt other pages even if page1 failed
	} else {
		for _, p := range page1Products {
			if !seenPInfIDs[p.ProductInfoID] {
				allProducts = append(allProducts, p)
				seenPInfIDs[p.ProductInfoID] = true
			}
		}
	}

	// Concurrent fetch for pages 2..PAGES_TO_SCAN
	var wg sync.WaitGroup
	resultsCh := make(chan []Product, PAGES_TO_SCAN-1)

	for i := 2; i <= PAGES_TO_SCAN; i++ {
		i := i
		wg.Add(1)
		go func(page int) {
			defer wg.Done()
			pagingURL := fmt.Sprintf(API_URL_PAGING_TEMPLATE, page)
			prods, err := fetchAndParseAPI(pagingURL)
			if err != nil {
				log.Printf("❌ Error fetching page %d: %v", page, err)
				return
			}
			if len(prods) == 0 {
				return
			}
			resultsCh <- prods
		}(i)
	}

	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	for prods := range resultsCh {
		for _, p := range prods {
			if !seenPInfIDs[p.ProductInfoID] {
				allProducts = append(allProducts, p)
				seenPInfIDs[p.ProductInfoID] = true
			}
		}
	}

	return allProducts, nil
}

// -----------------------
// BOT LOGIC
// -----------------------
func initializeBaseline() {
	log.Println("No baseline file found. Performing definitive multi-API scan...")
	products, err := getAllProductsFromAPI()
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
	log.Printf("✅ Baseline created with %d IN-STOCK items.", len(initialItems))
}

func checkForNewItems() []Product {
	log.Printf("🔎 (%s) Performing definitive multi-API check...", time.Now().Format("15:04:05"))
	var newProductsFound []Product
	allProducts, err := getAllProductsFromAPI()
	if err != nil {
		log.Printf("❌ Error getting all products: %v", err)
		sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("⚠️ Bot encountered an error: %v", err))
		return newProductsFound
	}

	log.Printf("... Total products/variations found this check: %d", len(allProducts))
	for _, p := range allProducts {
		if p.StockStatus == "0" {
			continue
		}

		uniqueID := p.ProductInfoID

		seenItemsMu.RLock()
		seen := seenItems[uniqueID]
		seenItemsMu.RUnlock()

		if !seen {
			log.Printf("🚨 NEW ITEM FOUND: %s", p.ProductName)
			newProductsFound = append(newProductsFound, p)

			fullURL := constructFullURL(p)
			message := fmt.Sprintf(
				"<b>🔥 New Hot Wheels Listing!</b>\n\n<b>Name:</b> %s\n<b>Price:</b> %s\n\n<b>Link:</b> <a href='%s'>Click Here</a>",
				p.ProductName, p.Price, fullURL,
			)
			sendTelegramMessage(TELEGRAM_CHAT_ID, message)

			// Save to disk & memory
			saveNewItem(uniqueID)
		}
	}
	return newProductsFound
}

func scraperWorker(stop chan struct{}) {
	initialFinds := checkForNewItems()
	mutex.Lock()
	checkHistory = append(checkHistory, CheckResult{Timestamp: time.Now(), FoundProducts: initialFinds})
	mutex.Unlock()
	if len(initialFinds) == 0 {
		log.Println("...No new items found (initial check).")
		sendTelegramMessage(TELEGRAM_CHAT_ID, "✅ No new listings found on initial check.")
	}
	for {
		mutex.Lock()
		interval := checkInterval
		paused := isPaused
		mutex.Unlock()

		select {
		case <-time.After(interval):
			if !paused {
				newlyFoundProducts := checkForNewItems()
				mutex.Lock()
				checkHistory = append(checkHistory, CheckResult{Timestamp: time.Now(), FoundProducts: newlyFoundProducts})
				if len(checkHistory) > 10 {
					checkHistory = checkHistory[1:]
				}
				isMuted := heartbeatMuted
				currentInterval := checkInterval
				mutex.Unlock()
				if len(newlyFoundProducts) == 0 {
					log.Println("...No new items found.")
					if !isMuted {
						sendTelegramMessage(TELEGRAM_CHAT_ID, fmt.Sprintf("✅ No new listings found. Next check in ~%.0f seconds.", currentInterval.Seconds()))
					}
				}
			} else {
				log.Println("...Scraper is paused.")
			}
		case <-stop:
			log.Println("Scraper worker shutting down.")
			return
		}
	}
}

// -----------------------
// Telegram command listener (polling)
// -----------------------
func commandListenerWorker(stop chan struct{}) {
	log.Println("🤖 Command listener started.")
	var lastUpdateID int
	for {
		apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", TELEGRAM_BOT_TOKEN, lastUpdateID+1)
		resp, err := httpClient.Get(apiURL)
		if err != nil {
			log.Printf("Error getting updates: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var updates TelegramUpdateResponse
		_ = json.Unmarshal(body, &updates) // ignore parse errors (skip)

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
				sendTelegramMessage(ADMIN_CHAT_ID, "▶️ Bot resumed.")
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
				sendTelegramMessage(ADMIN_CHAT_ID, "🔕 Heartbeat notifications muted.")
			case "/unmute":
				mutex.Lock()
				heartbeatMuted = false
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "🔔 Heartbeat notifications enabled.")
			case "/setinterval":
				if len(parts) > 1 {
					i, err := strconv.Atoi(parts[1])
					// allow minimum 5 seconds to match user's request
					if err == nil && i >= 5 {
						mutex.Lock()
						checkInterval = time.Duration(i) * time.Second
						mutex.Unlock()
						sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("✅ Interval set to %d seconds.", i))
					} else {
						sendTelegramMessage(ADMIN_CHAT_ID, "❌ Invalid interval. Minimum is 5 seconds.")
					}
				} else {
					sendTelegramMessage(ADMIN_CHAT_ID, "Usage: /setinterval <seconds>")
				}
			case "/status":
				mutex.Lock()
				status := "▶️ Running"
				if isPaused {
					status = "⏸️ Paused"
				}
				hbStatus := "🔔 Active"
				if heartbeatMuted {
					hbStatus = "🔕 Muted"
				}
				interval := checkInterval
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("<b>Bot Status:</b>\n%s\nCheck Interval: %.0f seconds\nHeartbeat: %s", status, interval.Seconds(), hbStatus))
			case "/recent":
				var sb strings.Builder
				sb.WriteString("<b>🔎 Recent Finds (Last 10 Checks)</b>\n\n")
				mutex.Lock()
				totalFound := 0
				for i := len(checkHistory) - 1; i >= 0; i-- {
					result := checkHistory[i]
					if len(result.FoundProducts) > 0 {
						totalFound += len(result.FoundProducts)
						loc, _ := time.LoadLocation("Asia/Kolkata")
						sb.WriteString(fmt.Sprintf("<b><u>Found at %s:</u></b>\n", result.Timestamp.In(loc).Format("03:04 PM, Jan 02")))
						for _, p := range result.FoundProducts {
							fullURL := constructFullURL(p)
							sb.WriteString(fmt.Sprintf("- <a href='%s'>%s</a>\n", fullURL, p.ProductName))
						}
						sb.WriteString("\n")
					}
				}
				mutex.Unlock()
				if totalFound == 0 {
					sb.WriteString("No new products found in the last 10 checks.")
				}
				sendTelegramMessage(ADMIN_CHAT_ID, sb.String())
			}
		}
	}
}

// -----------------------
// HTTP Server for Render /health, /status, /ping
// -----------------------
func startHTTPServer() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("🔥 Hot Wheels Hunter is running! 🔥"))
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		status := "running"
		if isPaused {
			status = "paused"
		}
		interval := checkInterval
		itemCount := 0
		seenItemsMu.RLock()
		itemCount = len(seenItems)
		seenItemsMu.RUnlock()
		mutex.Unlock()

		response := fmt.Sprintf(`{
			"status": "%s",
			"check_interval_seconds": %.0f,
			"tracked_items": %d,
			"bot": "Hot Wheels Hunter",
			"timestamp": "%s"
		}`, status, interval.Seconds(), itemCount, time.Now().Format("2006-01-02 15:04:05"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(response))
	})

	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	})

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	go func() {
		log.Printf("🌐 HTTP server starting on port %s", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("❌ HTTP server error: %v", err)
		}
	}()
}

// -----------------------
// MAIN
// -----------------------
func main() {
	// Logging
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("--- 🔥 Hot Wheels Hunter Started (Render-ready) 🔥 ---")

	// Start HTTP server for Render
	startHTTPServer()

	// Start keepalive self-pings (to your render URL)
	startKeepAlive()

	// Load baseline or create it
	if _, err := os.Stat(SEEN_ITEMS_FILE); os.IsNotExist(err) {
		initializeBaseline()
	}
	loadSeenItems()
	seenItemsMu.RLock()
	log.Printf("✅ Loaded baseline: %d items tracked.", len(seenItems))
	seenItemsMu.RUnlock()

	// Channels for graceful shutdown via /stop telegram command
	stop := make(chan struct{})

	// Start workers: scraper and telegram command listener
	go scraperWorker(stop)
	go commandListenerWorker(stop)

	// Notify admin
	sendTelegramMessage(ADMIN_CHAT_ID, "🚀 Bot is online and running!")

	// Block until stop channel is closed
	<-stop
	log.Println("--- Bot shutdown complete ---")
}
