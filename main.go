
			mutex.Lock()
			seenItems[uniqueID] = true
			mutex.Unlock()
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
		log.Println("...No new items found.")
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
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
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
					if err == nil && i >= 10 {
						mutex.Lock()
						checkInterval = time.Duration(i) * time.Second
						mutex.Unlock()
						sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("✅ Interval set to %d seconds.", i))
					} else {
						sendTelegramMessage(ADMIN_CHAT_ID, "❌ Invalid interval.")
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

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("--- 🔥 Hot Wheels Hunter Started (Variation-Aware API Version) 🔥 ---")

	// Add HTTP server for Render deployment
	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("🔥 Hot Wheels Hunter is running! 🔥"))
		})
		
		http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			mutex.Lock()
			status := "running"
			if isPaused {
				status = "paused"
			}
			interval := checkInterval
			itemCount := len(seenItems)
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
		
		http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("pong"))
		})
		
		log.Printf("🌐 HTTP server starting on port %s", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Printf("❌ HTTP server error: %v", err)
		}
	}()
	
	// Start keep-alive service
	startKeepAlive()
	
	if _, err := os.Stat(SEEN_ITEMS_FILE); os.IsNotExist(err) {
		initializeBaseline()
	}
	loadSeenItems()
	log.Printf("✅ Loaded existing baseline with %d items.", len(seenItems))
	stop := make(chan struct{})
	go scraperWorker(stop)
	go commandListenerWorker(stop)
	sendTelegramMessage(ADMIN_CHAT_ID, "🚀 Bot is online and running!")
	<-stop
	log.Println("--- Bot has been shut down. ---")
}
