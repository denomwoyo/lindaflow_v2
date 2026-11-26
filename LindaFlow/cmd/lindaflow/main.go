package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Port, DataFile, DarajaBaseURL, ConsumerKey, ConsumerSecret string
	ShortCode, PassKey, CallbackBaseURL, TransactionType string
}

type Transaction struct {
	ID string `json:"id"`
	MerchantID string `json:"merchant_id"`
	Phone string `json:"phone"`
	Amount float64 `json:"amount"`
	Currency string `json:"currency"`
	Direction string `json:"direction"`
	Reference string `json:"reference"`
	Receipt string `json:"receipt,omitempty"`
	Status string `json:"status"`
	RiskScore int `json:"risk_score"`
	RiskLevel string `json:"risk_level"`
	Decision string `json:"decision"`
	Reasons []string `json:"reasons"`
	Offline bool `json:"offline"`
	CreatedAt time.Time `json:"created_at"`
}

type Event struct {
	MerchantID string `json:"merchant_id"`
	Phone string `json:"phone"`
	Amount float64 `json:"amount"`
	Currency string `json:"currency"`
	Direction string `json:"direction"`
	Reference string `json:"reference"`
	Receipt string `json:"receipt"`
	DeviceID string `json:"device_id"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct {
	mu sync.RWMutex
	file *os.File
	transactions []Transaction
}

type App struct {
	cfg Config
	store *Store
	client *http.Client
	tokenMu sync.Mutex
	token string
	tokenExpiry time.Time
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" { return v }
	return fallback
}

func loadConfig() Config {
	return Config{
		Port: env("PORT", "8080"),
		DataFile: env("DATA_FILE", "data/lindaflow.jsonl"),
		DarajaBaseURL: strings.TrimRight(env("DARAJA_BASE_URL", "https://sandbox.safaricom.co.ke"), "/"),
		ConsumerKey: os.Getenv("DARAJA_CONSUMER_KEY"),
		ConsumerSecret: os.Getenv("DARAJA_CONSUMER_SECRET"),
		ShortCode: os.Getenv("MPESA_SHORTCODE"),
		PassKey: os.Getenv("MPESA_PASSKEY"),
		CallbackBaseURL: strings.TrimRight(os.Getenv("CALLBACK_BASE_URL"), "/"),
		TransactionType: env("MPESA_TRANSACTION_TYPE", "CustomerPayBillOnline"),
	}
}

func openStore(path string) (*Store, error) {
	if err := os.MkdirAll("data", 0700); err != nil { return nil, err }
	if i := strings.LastIndex(path, "/"); i >= 0 {
		if err := os.MkdirAll(path[:i], 0700); err != nil { return nil, err }
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil { return nil, err }
	s := &Store{file:f}
	if _, err := f.Seek(0, 0); err != nil { return nil, err }
	dec := json.NewDecoder(f)
	for {
		var tx Transaction
		if err := dec.Decode(&tx); err != nil {
			if errors.Is(err, context.Canceled) { break }
			break
		}
		s.transactions = append(s.transactions, tx)
	}
	_, _ = f.Seek(0, 2)
	return s, nil
}

func (s *Store) add(tx Transaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(tx)
	if err != nil { return err }
	if _, err = s.file.Write(append(b, '\n')); err != nil { return err }
	s.transactions = append([]Transaction{tx}, s.transactions...)
	return nil
}

func (s *Store) list() []Transaction {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Transaction, len(s.transactions))
	copy(out, s.transactions)
	return out
}

func (s *Store) recentFor(phone string, since time.Time) []Transaction {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Transaction
	for _, tx := range s.transactions {
		if tx.Phone == phone && tx.CreatedAt.After(since) { out = append(out, tx) }
	}
	return out
}

func (a *App) score(e Event) (int, []string) {
	score := 0
	var reasons []string
	if e.Amount <= 0 { score += 100; reasons = append(reasons, "Invalid transaction amount") }
	if e.Amount >= 100000 { score += 25; reasons = append(reasons, "Unusually high transaction amount") }
	if e.Amount >= 50000 && e.Amount < 100000 { score += 12; reasons = append(reasons, "High transaction amount") }
	now := time.Now()
	if e.CreatedAt.Hour() < 5 || e.CreatedAt.Hour() >= 23 {
		score += 10; reasons = append(reasons, "Unusual transaction hour")
	}
	recent := a.store.recentFor(e.Phone, now.Add(-10*time.Minute))
	if len(recent) >= 5 {
		score += 30; reasons = append(reasons, "High transaction velocity")
	} else if len(recent) >= 3 {
		score += 15; reasons = append(reasons, "Elevated transaction velocity")
	}
	var total float64
	for _, tx := range recent { total += tx.Amount }
	if total+e.Amount >= 150000 {
		score += 20; reasons = append(reasons, "High cumulative value in 10-minute window")
	}
	if e.Phone == "" { score += 100; reasons = append(reasons, "Missing sender phone number") }
	if e.MerchantID == "" { score += 10; reasons = append(reasons, "Missing merchant identifier") }
	if score > 100 { score = 100 }
	return score, reasons
}

func decision(score int) (string, string) {
	switch {
	case score >= 80: return "critical", "hold"
	case score >= 55: return "high", "verify"
	case score >= 30: return "medium", "verify"
	default: return "low", "allow"
	}
}

func (a *App) saveEvent(e Event, offline bool) (Transaction, error) {
	if e.CreatedAt.IsZero() { e.CreatedAt = time.Now().UTC() }
	score, reasons := a.score(e)
	level, action := decision(score)
	tx := Transaction{
		ID: fmt.Sprintf("lf_%d", time.Now().UnixNano()),
		MerchantID:e.MerchantID, Phone:e.Phone, Amount:e.Amount,
		Currency:env("CURRENCY", "KES"), Direction:e.Direction,
		Reference:e.Reference, Receipt:e.Receipt, Status:"recorded",
		RiskScore:score, RiskLevel:level, Decision:action,
		Reasons:reasons, Offline:offline, CreatedAt:e.CreatedAt,
	}
	if err := a.store.add(tx); err != nil { return tx, err }
	return tx, nil
}

func (a *App) darajaToken(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.token != "" && time.Now().Before(a.tokenExpiry) { return a.token, nil }
	if a.cfg.ConsumerKey == "" || a.cfg.ConsumerSecret == "" {
		return "", errors.New("Daraja credentials are not configured")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", a.cfg.DarajaBaseURL+"/oauth/v1/generate?grant_type=client_credentials", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.cfg.ConsumerKey+":"+a.cfg.ConsumerSecret)))
	resp, err := a.client.Do(req)
	if err != nil { return "", err }
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK { return "", fmt.Errorf("Daraja OAuth returned HTTP %d", resp.StatusCode) }
	var result struct { AccessToken string `json:"access_token"`; ExpiresIn string `json:"expires_in"` }
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil { return "", err }
	seconds, _ := strconv.Atoi(result.ExpiresIn)
	if seconds < 60 { seconds = 3600 }
	a.token, a.tokenExpiry = result.AccessToken, time.Now().Add(time.Duration(seconds-30)*time.Second)
	return a.token, nil
}

func (a *App) darajaPost(ctx context.Context, path string, body any) (map[string]any, error) {
	token, err := a.darajaToken(ctx)
	if err != nil { return nil, err }
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", a.cfg.DarajaBaseURL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil { return nil, err }
	if resp.StatusCode < 200 || resp.StatusCode >= 300 { return result, fmt.Errorf("Daraja returned HTTP %d", resp.StatusCode) }
	return result, nil
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"service":"LindaFlow","status":"online","mode":"local-first","time":time.Now().UTC()})
}

func (a *App) transactions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET": writeJSON(w, 200, a.store.list())
	case "POST":
		var e Event
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&e); err != nil { http.Error(w,"Invalid JSON",400); return }
		tx, err := a.saveEvent(e, false)
		if err != nil { http.Error(w,"Could not persist transaction",500); return }
		writeJSON(w, 201, tx)
	default: http.Error(w,"Method not allowed",405)
	}
}

func (a *App) assess(w http.ResponseWriter, r *http.Request) {
	var e Event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&e); err != nil { http.Error(w,"Invalid JSON",400); return }
	score, reasons := a.score(e)
	level, action := decision(score)
	writeJSON(w,200,map[string]any{"risk_score":score,"risk_level":level,"decision":action,"reasons":reasons})
}

func (a *App) stkPush(w http.ResponseWriter, r *http.Request) {
	if a.cfg.CallbackBaseURL == "" || a.cfg.ShortCode == "" || a.cfg.PassKey == "" {
		http.Error(w,"Set CALLBACK_BASE_URL, MPESA_SHORTCODE and MPESA_PASSKEY",503); return
	}
	var in struct { Phone string `json:"phone"`; Amount int `json:"amount"`; AccountReference string `json:"account_reference"`; Description string `json:"description"`; MerchantID string `json:"merchant_id"` }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Phone == "" || in.Amount < 1 {
		http.Error(w,"Valid phone and positive amount are required",400); return
	}
	e := Event{MerchantID:in.MerchantID,Phone:in.Phone,Amount:float64(in.Amount),Direction:"inbound",Reference:in.AccountReference,CreatedAt:time.Now().UTC()}
	score, reasons := a.score(e)
	if score >= 80 {
		writeJSON(w, 202, map[string]any{"decision":"hold","risk_score":score,"reasons":reasons,"message":"STK Push not initiated because local risk policy placed it on hold"})
		return
	}
	timestamp := time.Now().Format("20060102150405")
	password := base64.StdEncoding.EncodeToString([]byte(a.cfg.ShortCode+a.cfg.PassKey+timestamp))
	payload := map[string]any{
		"BusinessShortCode":a.cfg.ShortCode,"Password":password,"Timestamp":timestamp,
		"TransactionType":a.cfg.TransactionType,"Amount":in.Amount,"PartyA":in.Phone,
		"PartyB":a.cfg.ShortCode,"PhoneNumber":in.Phone,
		"CallBackURL":a.cfg.CallbackBaseURL+"/api/mpesa/stk-callback",
		"AccountReference":in.AccountReference,"TransactionDesc":in.Description,
	}
	result, err := a.darajaPost(r.Context(),"/mpesa/stkpush/v1/processrequest",payload)
	if err != nil { writeJSON(w,502,map[string]any{"error":err.Error(),"provider_response":result}); return }
	writeJSON(w,200,result)
}

func (a *App) stkCallback(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil { http.Error(w,"Invalid callback",400); return }
	raw, _ := json.Marshal(body)
	var parsed struct {
		Body struct { Callback struct {
			CheckoutRequestID string `json:"CheckoutRequestID"`
			ResultCode int `json:"ResultCode"`
			ResultDesc string `json:"ResultDesc"`
			Metadata struct { Items []struct { Name string `json:"Name"`; Value any `json:"Value"` } `json:"Item"` } `json:"CallbackMetadata"`
		} `json:"stkCallback"` } `json:"Body"`
	}
	_ = json.Unmarshal(raw,&parsed)
	e := Event{Direction:"inbound",Currency:"KES",CreatedAt:time.Now().UTC()}
	for _, item := range parsed.Body.Callback.Metadata.Items {
		switch item.Name {
		case "MpesaReceiptNumber": e.Receipt = fmt.Sprint(item.Value)
		case "PhoneNumber": e.Phone = fmt.Sprint(item.Value)
		case "Amount": if v,ok:=item.Value.(float64); ok { e.Amount = v }
		}
	}
	e.Reference = parsed.Body.Callback.CheckoutRequestID
	tx, err := a.saveEvent(e,false)
	if err != nil { http.Error(w,"Persistence failure",500); return }
	tx.Status = "completed"
	if parsed.Body.Callback.ResultCode != 0 { tx.Status = "failed" }
	_ = a.store.add(tx)
	writeJSON(w,200,map[string]string{"ResultCode":"0","ResultDesc":"Accepted"})
}

func (a *App) registerC2B(w http.ResponseWriter, r *http.Request) {
	if a.cfg.ShortCode == "" || a.cfg.CallbackBaseURL == "" { http.Error(w,"Daraja shortcode and callback URL are required",503); return }
	payload := map[string]any{"ShortCode":a.cfg.ShortCode,"ResponseType":"Completed","ConfirmationURL":a.cfg.CallbackBaseURL+"/api/mpesa/c2b-confirmation","ValidationURL":a.cfg.CallbackBaseURL+"/api/mpesa/c2b-validation"}
	result, err := a.darajaPost(r.Context(),"/mpesa/c2b/v1/registerurl",payload)
	if err != nil { writeJSON(w,502,map[string]any{"error":err.Error(),"provider_response":result}); return }
	writeJSON(w,200,result)
}

func (a *App) c2bValidation(w http.ResponseWriter, r *http.Request) {
	var p map[string]any
	_ = json.NewDecoder(r.Body).Decode(&p)
	amount, _ := p["TransAmount"].(float64)
	phone := fmt.Sprint(p["MSISDN"])
	e := Event{Phone:phone,Amount:amount,Direction:"inbound",Reference:fmt.Sprint(p["BillRefNumber"]),CreatedAt:time.Now().UTC()}
	score, reasons := a.score(e)
	if score >= 80 {
		writeJSON(w,200,map[string]string{"ResultCode":"C2B00011","ResultDesc":"Transaction requires review"})
		return
	}
	_ = reasons
	writeJSON(w,200,map[string]string{"ResultCode":"0","ResultDesc":"Accepted"})
}

func (a *App) c2bConfirmation(w http.ResponseWriter, r *http.Request) {
	var p map[string]any
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil { http.Error(w,"Invalid callback",400); return }
	amount, _ := p["TransAmount"].(float64)
	e := Event{Phone:fmt.Sprint(p["MSISDN"]),Amount:amount,Direction:"inbound",Reference:fmt.Sprint(p["BillRefNumber"]),Receipt:fmt.Sprint(p["TransID"]),CreatedAt:time.Now().UTC()}
	_, err := a.saveEvent(e,false)
	if err != nil { http.Error(w,"Persistence failure",500); return }
	writeJSON(w,200,map[string]string{"ResultCode":"0","ResultDesc":"Accepted"})
}

func (a *App) stkQuery(w http.ResponseWriter, r *http.Request) {
	var in struct { CheckoutRequestID string `json:"checkout_request_id"` }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.CheckoutRequestID == "" { http.Error(w,"checkout_request_id required",400); return }
	ts := time.Now().Format("20060102150405")
	password := base64.StdEncoding.EncodeToString([]byte(a.cfg.ShortCode+a.cfg.PassKey+ts))
	result, err := a.darajaPost(r.Context(),"/mpesa/stkpushquery/v1/query",map[string]any{
		"BusinessShortCode":a.cfg.ShortCode,"Password":password,"Timestamp":ts,"CheckoutRequestID":in.CheckoutRequestID,
	})
	if err != nil { writeJSON(w,502,map[string]any{"error":err.Error(),"provider_response":result}); return }
	writeJSON(w,200,result)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type","application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	cfg := loadConfig()
	store, err := openStore(cfg.DataFile)
	if err != nil { log.Fatal(err) }
	app := &App{cfg:cfg,store:store,client:&http.Client{Timeout:8*time.Second}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health",app.health)
	mux.HandleFunc("/api/transactions",app.transactions)
	mux.HandleFunc("/api/assess",app.assess)
	mux.HandleFunc("/api/mpesa/stk-push",app.stkPush)
	mux.HandleFunc("/api/mpesa/stk-query",app.stkQuery)
	mux.HandleFunc("/api/mpesa/stk-callback",app.stkCallback)
	mux.HandleFunc("/api/mpesa/register-c2b",app.registerC2B)
	mux.HandleFunc("/api/mpesa/c2b-validation",app.c2bValidation)
	mux.HandleFunc("/api/mpesa/c2b-confirmation",app.c2bConfirmation)
	mux.Handle("/",http.FileServer(http.Dir("web")))
	server := &http.Server{Addr:":"+cfg.Port,Handler:securityHeaders(mux),ReadHeaderTimeout:3*time.Second}
	log.Printf("LindaFlow listening on :%s",cfg.Port)
	log.Fatal(server.ListenAndServe())
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		w.Header().Set("X-Content-Type-Options","nosniff")
		w.Header().Set("Referrer-Policy","no-referrer")
		w.Header().Set("X-Frame-Options","DENY")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-LindaFlow-Client")),[]byte(os.Getenv("LINDAFLOW_CLIENT_TOKEN"))) != 1 && os.Getenv("LINDAFLOW_CLIENT_TOKEN") != "" && strings.HasPrefix(r.URL.Path,"/api/") && !strings.HasSuffix(r.URL.Path,"callback") && !strings.Contains(r.URL.Path,"validation") {
			http.Error(w,"Unauthorized",401); return
		}
		next.ServeHTTP(w,r)
	})
}
