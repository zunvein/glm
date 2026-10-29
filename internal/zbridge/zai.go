// Z.AI wire protocol: request signing, guest auth / session init, chat
// completion calls (blocking + streaming), SSE parsing, and chat deletion.

package zbridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// ============================================================================
// Z.AI SIGNATURE GENERATION
// ============================================================================

func generateZaSignature(prompt, token, userID string) (signature, timestamp, urlParams string) {
	tsMs := time.Now().UnixMilli()
	timestamp = strconv.FormatInt(tsMs, 10)
	requestId := randomUUID()
	bucket := tsMs / 300000

	mac := hmac.New(sha256.New, []byte(session.SaltKey))
	mac.Write([]byte(strconv.FormatInt(bucket, 10)))
	wKey := hex.EncodeToString(mac.Sum(nil))

	type kv struct{ k, v string }
	payloadDict := []kv{
		{"requestId", requestId},
		{"timestamp", timestamp},
		{"user_id", userID},
	}
	sort.Slice(payloadDict, func(i, j int) bool {
		return payloadDict[i].k < payloadDict[j].k
	})
	var parts []string
	for _, p := range payloadDict {
		parts = append(parts, p.k+","+p.v)
	}
	sortedPayload := strings.Join(parts, ",")

	promptB64 := base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(prompt)))
	dataToSign := sortedPayload + "|" + promptB64 + "|" + timestamp

	mac2 := hmac.New(sha256.New, []byte(wKey))
	mac2.Write([]byte(dataToSign))
	signature = hex.EncodeToString(mac2.Sum(nil))

	params := url.Values{}
	params.Set("timestamp", timestamp)
	params.Set("requestId", requestId)
	params.Set("user_id", userID)
	params.Set("version", "0.0.1")
	params.Set("platform", "web")
	params.Set("token", token)
	params.Set("user_agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0.0.0")
	params.Set("language", "en-US")
	params.Set("screen_resolution", "1920x1080")
	params.Set("viewport_size", "1920x1080")
	params.Set("timezone", "Europe/Paris")
	params.Set("timezone_offset", "-60")
	params.Set("signature_timestamp", timestamp)
	urlParams = params.Encode()

	return
}

// ============================================================================
// JWT DECODE
// ============================================================================

func decodeJWT(token string) (id, name string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	decoded, err := base64Decode(parts[1])
	if err != nil {
		return "", ""
	}
	var data map[string]interface{}
	if err := json.Unmarshal(decoded, &data); err != nil {
		return "", ""
	}
	id, _ = data["id"].(string)
	email, _ := data["email"].(string)
	name = "Guest"
	if email != "" {
		name = strings.Split(email, "@")[0]
	}
	return id, name
}

// ============================================================================
// Z.AI SESSION INITIALIZATION
// ============================================================================

func scrapeConfig() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", BASE_URL, nil)
	if err != nil {
		log.Printf("[Config] Scrape error: %s, using default feVersion", err.Error())
		return
	}
	resp, err := zaiHTTPClient.Do(req)
	if err != nil {
		log.Printf("[Config] Scrape error: %s, using default feVersion", err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if match := feVersionRe.FindString(string(body)); match != "" {
		session.mu.Lock()
		session.FeVersion = match
		session.mu.Unlock()
		log.Printf("[Config] fe_version: %s", match)
	}
}

func initializeSession() error {
	session.mu.Lock()
	if session.Initializing {
		session.mu.Unlock()
		for {
			time.Sleep(100 * time.Millisecond)
			session.mu.Lock()
			if !session.Initializing {
				session.mu.Unlock()
				return nil
			}
			session.mu.Unlock()
		}
	}
	session.Initializing = true
	session.mu.Unlock()

	defer func() {
		session.mu.Lock()
		session.Initializing = false
		session.mu.Unlock()
	}()

	if len(config.ZaiTokens) > 0 || config.ZaiToken != "" {
		// Multi-account mode: round-robin across ZAI_TOKENS; single ZAI_TOKEN
		// falls through to the legacy path below.
		if len(config.ZaiTokens) > 0 {
			// Select the account pointed at by the current round-robin
			// cursor WITHOUT advancing it — advancing is the sole job of
			// selectZAIAccount (proactive per-request rotation, issue #47).
			// Keeping the cursor stable here makes initializeSession
			// idempotent w.r.t. rotation, so a re-entry triggered by
			// initialization after a rotation does not double-advance and
			// skip to a wrong account.
			session.mu.Lock()
			idx := session.tokenIndex % len(config.ZaiTokens)
			tok := config.ZaiTokens[idx]
			session.mu.Unlock()
			session.Token = tok
		} else {
			session.Token = config.ZaiToken
		}
		log.Println("[Session] Using configured ZAI token(s), skipping guest init.")
		id, name := decodeJWT(session.Token)
		session.UserID = id
		if name != "" {
			session.UserName = name
		}
		if session.UserID == "" {
			session.UserName = "User"
		}
		uidPreview := session.UserID
		if len(uidPreview) > 8 {
			uidPreview = uidPreview[:8]
		}
		log.Printf("[Session] Token user: %s... (%s)", uidPreview, session.UserName)
		session.Initialized = true
		return nil
	}

	log.Println("[Session] Initializing Z.AI session...")

	scrapeConfig()

	headers := map[string]string{
		"Origin":       BASE_URL,
		"Referer":      BASE_URL + "/",
		"User-Agent":   zaiUserAgent,
		"Content-Type": "application/json",
	}

	// Initial guest POST (fire-and-forget)
	ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel1()
	req1, _ := http.NewRequestWithContext(ctx1, "POST", BASE_URL+"/api/v1/auths/guest", strings.NewReader("{}"))
	for k, v := range headers {
		req1.Header.Set(k, v)
	}
	zaiHTTPClient.Do(req1)

	// GET /api/v1/auths/
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	req2, _ := http.NewRequestWithContext(ctx2, "GET", BASE_URL+"/api/v1/auths/", nil)
	for k, v := range headers {
		req2.Header.Set(k, v)
	}
	resp, err := zaiHTTPClient.Do(req2)
	if err != nil {
		log.Printf("[Session] Initialization error: %s", err.Error())
		session.Initialized = false
		return err
	}

	if resp.StatusCode != 200 {
		resp.Body.Close()
		err := fmt.Errorf("Auth failed: %d", resp.StatusCode)
		log.Printf("[Session] Initialization error: %s", err.Error())
		session.Initialized = false
		return err
	}

	var authData struct {
		Token string `json:"token"`
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	json.Unmarshal(body, &authData)
	session.Token = authData.Token

	if session.Token == "" {
		ctx3, cancel3 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel3()
		req3, _ := http.NewRequestWithContext(ctx3, "POST", BASE_URL+"/api/v1/auths/guest", strings.NewReader("{}"))
		for k, v := range headers {
			req3.Header.Set(k, v)
		}
		guestResp, err := zaiHTTPClient.Do(req3)
		if err == nil {
			var gd struct {
				Token string `json:"token"`
			}
			gb, _ := io.ReadAll(guestResp.Body)
			guestResp.Body.Close()
			json.Unmarshal(gb, &gd)
			session.Token = gd.Token
		}
	}

	if session.Token != "" {
		id, name := decodeJWT(session.Token)
		session.UserID = id
		if name != "" {
			session.UserName = name
		}
		uidPreview := session.UserID
		if len(uidPreview) > 8 {
			uidPreview = uidPreview[:8]
		}
		log.Printf("[Session] Connected. UserID: %s... (%s)", uidPreview, session.UserName)
		session.Initialized = true
		return nil
	}

	session.Initialized = false
	return errors.New("No token received from Z.AI")
}

// ============================================================================
// Z.AI COMMUNICATION
// ============================================================================

func sendToZAI(prompt string, opts SendOptions) (<-chan ZAIResult, error) {
	session.mu.Lock()
	defaultChatID := session.ChatID
	defaultMessages := session.Messages
	initialized := session.Initialized
	session.mu.Unlock()

	model := opts.Model
	if model == "" {
		model = "glm-5"
	}

	// Resolve features dynamically from per-model state
	// (server defaults + stored user overrides)
	featuresMap := resolveFeaturesForModel(model)

	// Apply per-request overrides (highest precedence).
	//
	// Issue #42: Z.AI's completions payload has TWO search-related feature
	// keys, but only one of them does anything:
	//   - auto_web_search — the real toggle for the model's built-in
	//     WebSearch feature (the globe icon on chat.z.ai)
	//   - web_search      — always false upstream; sending true here does
	//     NOT enable anything (it's just mirrored back by the server)
	// So the webSearch request flag now toggles auto_web_search only, and
	// web_search is pinned to false in every request (see the payload
	// build in sendToZAIStream). "Advanced Search" on chat.z.ai is a
	// different thing entirely — it's an MCP server, opted into per
	// request via the top-level "mcp_servers": ["advanced-search"] field
	// (see SendOptions.AdvancedSearch).
	webSearch := false
	advancedSearch := false
	if opts.WebSearch != nil {
		webSearch = *opts.WebSearch
	}
	if opts.AdvancedSearch != nil {
		advancedSearch = *opts.AdvancedSearch
	}
	if config.AgentMode {
		// Issue #42 gate: agent mode folds tools & roles into a single
		// user-only prompt. Z.AI serves the built-in WebSearch through an
		// internal tool-call loop on its side; mixing that with the agent
		// shim's own tool contract confuses the model (it starts emitting
		// internal tool_call markers like retrieve/open_url as if they were
		// the caller's tools, or refuses to search at all). So: agent mode
		// always wins and web search stays off, even when the request
		// asked for it.
		if webSearch || advancedSearch {
			logInfo("[web_search] agent mode active — disabling web search for this request")
		}
		webSearch = false
		advancedSearch = false
	}
	if webSearch {
		featuresMap["auto_web_search"] = true
	} else if opts.WebSearch != nil || config.AgentMode {
		// Only strip auto_web_search when the request explicitly turned it
		// off, or when the agent-mode gate forces it off — otherwise the
		// stored per-model overrides (POST /features) keep working.
		delete(featuresMap, "auto_web_search")
	}
	// web_search is never set to true upstream — auto_web_search is the
	// only working toggle (issue #42). The payload build pins it to false.
	delete(featuresMap, "web_search")
	if opts.Thinking != nil {
		featuresMap["enable_thinking"] = *opts.Thinking
	}
	if opts.ImageGen != nil {
		featuresMap["image_generation"] = *opts.ImageGen
	}
	if opts.PreviewMode != nil {
		featuresMap["preview_mode"] = *opts.PreviewMode
	}

	// ── reasoning_effort handling ──
	// Defensive: always strip any stale reasoning_effort first so unsupported
	// models never receive a placeholder value (would cause malfunction).
	delete(featuresMap, "reasoning_effort")

	if opts.ReasoningEffort != "" {
		if modelSupportsReasoningEffort(model) {
			if isValidReasoningEffort(opts.ReasoningEffort) {
				// Forward reasoning_effort INSIDE the features payload
				featuresMap["reasoning_effort"] = opts.ReasoningEffort
				// When reasoning_effort is active, enable_thinking MUST be true
				// and any user modification on enable_thinking is ignored.
				featuresMap["enable_thinking"] = true
				logInfo(fmt.Sprintf(
					"[reasoning_effort] model=%s effort=%s enabled (enable_thinking forced true)",
					model, opts.ReasoningEffort))
			} else {
				logError(fmt.Sprintf(
					"[reasoning_effort] invalid value '%s' for model=%s (accepted: high, max); ignored",
					opts.ReasoningEffort, model))
			}
		} else {
			logInfo(fmt.Sprintf(
				"[reasoning_effort] model=%s does not support reasoning_effort; parameter ignored",
				model))
		}
	}

	// Remove 'think' entirely; ALWAYS force image_generation to false
	delete(featuresMap, "think")
	featuresMap["image_generation"] = false

	chatID := opts.ChatID
	if chatID == "" {
		chatID = defaultChatID
	}
	messages := opts.Messages
	if messages == nil {
		messages = defaultMessages
	}

	if !initialized {
		if err := initializeSession(); err != nil {
			return nil, err
		}
	}

	resolvedOpts := struct {
		Model, ChatID     string
		FeaturesMap       map[string]interface{}
		Messages          []Message
		ClientMessagesRaw json.RawMessage
		Files             []map[string]interface{}
		RequestID         string
		AdvancedSearch    bool
	}{
		Model:             model,
		ChatID:            chatID,
		FeaturesMap:       featuresMap,
		Messages:          messages,
		ClientMessagesRaw: opts.ClientMessagesRaw,
		Files:             opts.Files,
		RequestID:         opts.RequestID,
		AdvancedSearch:    advancedSearch,
	}

	ch := make(chan ZAIResult, 100)
	go func() {
		defer close(ch)
		err := sendToZAIStream(prompt, resolvedOpts, ch)
		if err != nil {
			ch <- ZAIResult{Err: err}
		}
	}()
	return ch, nil
}

func sendToZAIStream(prompt string, opts struct {
	Model, ChatID     string
	FeaturesMap       map[string]interface{}
	Messages          []Message
	ClientMessagesRaw json.RawMessage
	Files             []map[string]interface{}
	RequestID         string
	AdvancedSearch    bool
}, ch chan<- ZAIResult) error {
	// Pacing gate (WAF relief): every upstream chat path funnels through
	// this function, so one gate here covers stream + non-stream callers.
	// Cap rejection maps to HTTP 429 via statusFromError.
	if err := acquirePace(); err != nil {
		return err
	}

	// Issue #41/#46: retry budget. The existing loop is limited to 2 attempts
	// (only retrying on HTTP 401 session re-init). For MODEL_CONCURRENCY_LIMIT
	// ("Model is currently at capacity"), a per-account per-model burst limit,
	// we want extra attempts with backoff and, when multiple ZAI_TOKENS are
	// configured, round-robin rotation to the next account. Cap the total so
	// we never blow past the handler timeout:
	//   timeoutPerRequest = config.Timeouts.Default * 2 ms
	//   total budget       = 5 round-trips (initial + up to 4 retries), each
	//                        with an incremental backoff 0.5–1.5 s before it.
	const maxAttempts = 5

	// Issue #47: proactive per-request account rotation. Instead of only
	// swapping accounts when a concurrency/rate error forces a retry, we
	// advance to the next Z.AI account on EVERY incoming request so the load
	// is spread evenly across all ZAI_TOKENS accounts (avoids hammering a
	// single account during bursts). This is a single no-op with one token.
	// We advance only on the first attempt of the loop; retry-time rotation
	// (rotateToken below) continues to advance on subsequent attempts, so we
	// never skip two accounts for a single error.
	nextZAIAccount()

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Issue #41: while the breaker believes the egress IP is blocked by
		// the Aliyun WAF, do not spend a captcha (each costs a harvested
		// device token and ~10-15 s of signing) on a request that is
		// guaranteed to be answered with the block page. The handler-level
		// gate (RejectIfWAFBlocked) already rejects most requests before
		// this point; this second check catches requests that were already
		// in flight when the block was detected. A background prober
		// re-checks the block and reopens traffic automatically.
		if err, _ := CheckWAF(); err != nil {
			return err
		}

		session.mu.Lock()
		token := session.Token
		userID := session.UserID
		feVersion := session.FeVersion
		session.mu.Unlock()

		signature, _, urlParams := generateZaSignature(prompt, token, userID)
		urlStr := BASE_URL + "/api/v2/chat/completions?" + urlParams

		var messagesField interface{}
		if len(opts.ClientMessagesRaw) > 0 {
			messagesField = json.RawMessage(opts.ClientMessagesRaw)
		} else {
			forwarded := make([]Message, 0, len(opts.Messages)+1)
			forwarded = append(forwarded, opts.Messages...)
			promptJSON, _ := json.Marshal(prompt)
			forwarded = append(forwarded, Message{Role: "user", Content: json.RawMessage(promptJSON)})
			messagesField = forwarded
		}

		captchaParam, err := getCaptchaVerifyParam()
		if err != nil {
			return err
		}

		// Build features payload from dynamically resolved per-model features.
		// reasoning_effort is only present in opts.FeaturesMap if the model supports
		// it AND a valid value was provided — no placeholder is added for
		// unsupported models (would cause malfunction).
		featuresPayload := make(map[string]interface{})
		for k, v := range opts.FeaturesMap {
			featuresPayload[k] = v
		}
		// Remove 'think' entirely — only enable_thinking reaches the request
		delete(featuresPayload, "think")
		featuresPayload["flags"] = []interface{}{}
		// image_generation is ALWAYS false
		featuresPayload["image_generation"] = false
		// web_search is always false — auto_web_search is the only working
		// web-search toggle upstream (issue #42).
		featuresPayload["web_search"] = false

		// Z.AI killed the "glm-5.3-flash" id in the fe-1.1.122 deploy
		// (2026-10-28); GLM-5.3-Flash is served as "x-preview-l" now.
		// Alias the dead id so older clients keep working (500 otherwise).
		upstreamModel := opts.Model
		if upstreamModel == "glm-5.3-flash" {
			upstreamModel = "x-preview-l"
		}
		// Z.AI requires a client-generated message id on every completion
		// (upstream 400 MISSING_MESSAGE_ID otherwise, observed 2026-10-28).
		// Same client-UUID pattern as chat_id — mint one per request.
		requestBody := map[string]interface{}{
			"id":                   randomUUID(),
			"model":                upstreamModel,
			"chat_id":              opts.ChatID,
			"messages":             messagesField,
			"signature_prompt":     prompt,
			"stream":               true,
			"captcha_verify_param": captchaParam,
			"features":             featuresPayload,
		}
		// Attach uploaded files (images) only when present — text-only
		// requests keep the exact body shape they always had.
		if len(opts.Files) > 0 {
			requestBody["files"] = opts.Files
		}
		// Issue #42: "Advanced Search" on chat.z.ai is an MCP server. When
		// enabled in the web UI, the frontend adds a top-level
		// "mcp_servers": ["advanced-search"] field to the completions
		// payload (same level as captcha_verify_param), and the model then
		// runs its searches through the advanced-search MCP tools
		// (internal tool calls like retrieve/open_url show up in the SSE
		// stream, answered server-side by Z.AI). Only attach the field when
		// it's on — a plain web-search request keeps the exact body shape
		// the web client sends.
		if opts.AdvancedSearch {
			requestBody["mcp_servers"] = []string{"advanced-search"}
		}

		bodyBytes, _ := json.Marshal(requestBody)

		if config.Logging.Level == "debug" {
			// PRIVACY (29 Okt, owner directive): never log raw chat payloads or
			// credentials. Values are reduced to sizes; bodies stay on the wire.
			log.Println("[DEBUG] Z.AI url", maskURLToken(urlStr))
			log.Printf("[DEBUG] Z.AI request body: %d bytes", len(bodyBytes))
			log.Printf("[DEBUG] Z.AI request headers: authorization=<redacted> content-type=application/json x-fe-Version=%s x-region=overseas x-signature=<redacted>", feVersion)
		}

		timeout := time.Duration(config.Timeouts.Default) * time.Millisecond * 2
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		req, err := http.NewRequestWithContext(ctx, "POST", urlStr, bytes.NewReader(bodyBytes))
		if err != nil {
			cancel()
			return fmt.Errorf("Z.AI connection error: %s", err.Error())
		}
		req.Header.Set("authorization", "Bearer "+token)
		req.Header.Set("User-Agent", zaiUserAgent)
		req.Header.Set("content-type", "application/json")
		req.Header.Set("Origin", BASE_URL)
		req.Header.Set("Referer", BASE_URL+"/")
		req.Header.Set("x-fe-Version", feVersion)
		req.Header.Set("x-region", "overseas")
		req.Header.Set("x-signature", signature)
		resp, err := zaiHTTPClient.Do(req)
		if err != nil {
			cancel()
			return fmt.Errorf("Z.AI connection error: %s", err.Error())
		}

		if config.Logging.Level == "debug" {
			log.Printf("[DEBUG] Z.AI response status: %d %s", resp.StatusCode, resp.Status)
			hdrs := map[string]string{}
			for k, v := range resp.Header {
				hdrs[k] = strings.Join(v, ", ")
			}
			hdrJSON, _ := json.MarshalIndent(hdrs, "", "  ")
			log.Println("[DEBUG] Z.AI response headers:", string(hdrJSON))
		}

		if resp.StatusCode == 401 {
			resp.Body.Close()
			cancel()
			session.mu.Lock()
			session.Initialized = false
			session.mu.Unlock()
			if err := initializeSession(); err != nil {
				return err
			}
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			if config.Logging.Level == "debug" {
				log.Println("[DEBUG] Z.AI error body:", string(errBody))
			}
			// Issue #41: the Aliyun WAF block page comes back as 405 with
			// the block HTML. Detect it, trip the breaker, and surface a
			// short actionable error instead of the raw 3 KB page.
			if isWAFBlockResponse(resp.StatusCode, errBody) {
				RecordWAFBlock()
				return fmt.Errorf("%w (retry in ~%s)", ErrWAFBlock, wafRetryHint().Round(time.Second))
			}
			RecordWAFSuccess() // a real API error proves the IP is not blocked
			errMsg := fmt.Sprintf("Z.AI error %d: %s", resp.StatusCode, truncateErrorBody(errBody))
			// Per-account concurrency/rate limit on the HTTP layer (429 or a
			// non-2xx message about capacity): rotate to the next ZAI_TOKENS
			// account and retry instead of surfacing immediately.
			if (resp.StatusCode == 429 || strings.Contains(errMsg, "at capacity")) && attempt < maxAttempts-1 {
				backoff := time.Duration(500+attempt*250) * time.Millisecond
				if config.Logging.Level == "debug" {
					log.Printf("[DEBUG] ZAI concurrency limit (HTTP %d), rotate+retry in %v: %s", resp.StatusCode, backoff, errMsg)
				}
				rotateToken()
				time.Sleep(backoff)
				continue
			}
			return errors.New(errMsg)
		}

		err = streamSSEResponse(resp.Body, ch, opts.RequestID)
		resp.Body.Close()
		cancel()
		if err == nil {
			// A healthy SSE stream is the strongest proof the IP is not
			// blocked; keep the breaker closed even if it was left open by
			// a stale observation.
			RecordWAFSuccess()
		}
		if isRetriableZAIError(err) && attempt < maxAttempts-1 {
			// Inline SSE error (HTTP 200, error inside body) — e.g.
			// MODEL_CONCURRENCY_LIMIT ("Model is currently at capacity"),
			// an RTM/rate/permission error. Back off, roll to the next
			// account (if any), and retry. Return the original error if we
			// still fail after exhausting the budget.
			backoff := time.Duration(500+attempt*250) * time.Millisecond
			log.Printf("[API] ZAI retriable error (attempt %d/%d), rotate+backoff %v: %s", attempt+1, maxAttempts, backoff, err.Error())
			rotateToken()
			time.Sleep(backoff)
			continue
		}
		return err
	}
	return errors.New("Max retries exceeded")
}

// selectZAIAccount advances the round-robin index one step and installs the
// corresponding token (and its decoded identity) as the active session
// account. With a single config.ZaiToken it is a no-op. It intentionally does
// NOT touch session.Initialized: for configured-token deployments there is no
// guest handshake to redo, so clearing it here would make initializeSession
// re-run on every request and — because init would select the same cursor
// again — could double-advance the rotation on re-entry (issue #47 fix).
// rotateToken keeps forcing a re-init, which is correct on the error/retry
// path where a fresh upstream handle is actually wanted.
func selectZAIAccount() {
	session.mu.Lock()
	n := len(config.ZaiTokens)
	if n > 1 {
		session.tokenIndex++
		idx := session.tokenIndex % n
		session.Token = config.ZaiTokens[idx]
		id, name := decodeJWT(session.Token)
		if id != "" {
			session.UserID = id
		}
		if name != "" {
			session.UserName = name
		}
	}
	session.mu.Unlock()
}

// nextZAIAccount is the proactive per-request rotation hook (issue #47). It
// advances the round-robin index so consecutive requests alternate across the
// configured ZAI_TOKENS accounts, spreading the upstream load evenly instead
// of letting one account absorb a burst. Rotation done on error (see
// rotateToken) advances again, which is exactly what we want — with the
// per-request advance happening first, an error-triggered retry only moves to
// the account that would have been next anyway.
func nextZAIAccount() {
	selectZAIAccount()
	if n := len(config.ZaiTokens); n > 1 {
		session.mu.Lock()
		idx := session.tokenIndex % n
		uid := session.UserID
		session.mu.Unlock()
		preview := uid
		if len(preview) > 8 {
			preview = preview[:8]
		}
		log.Printf("[API] per-request account -> idx %d/%d (%s)", idx, n, preview)
	}
}

// rotateToken advances the active Z.AI account to the next token in the
// ZAI_TOKENS pool (round-robin). With a single token it is a no-op. Used to
// spread load across accounts when hitting per-account per-model concurrency
// limits. The next account also forces a session re-init so its own token and
// user id are picked up on the following request.
func rotateToken() {
	session.mu.Lock()
	n := len(config.ZaiTokens)
	if n > 1 {
		session.tokenIndex++
		idx := session.tokenIndex % n
		session.Token = config.ZaiTokens[idx]
		id, _ := decodeJWT(session.Token)
		if id != "" {
			session.UserID = id
		}
		// Force re-init so the newly selected account's identity is used.
		session.Initialized = false
		log.Printf("[API] Rotated Z.AI account -> index %d/%d", session.tokenIndex%n, n)
	}
	session.mu.Unlock()
}

// isRetriableZAIError reports whether an error returned from the Z.AI call
// (inline SSE error body or HTTP-layer) is safe to retry after a brief backoff
// and optional account rotation. The dominant case is the per-account
// per-model concurrency limit MODEL_CONCURRENCY_LIMIT ("Model is currently at
// capacity"), seen during burst periods.
func isRetriableZAIError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if strings.Contains(s, "MODEL_CONCURRENCY_LIMIT") {
		return true
	}
	if strings.Contains(s, "at capacity") {
		return true
	}
	if strings.Contains(s, "rate limit") || strings.Contains(s, "RateLimit") {
		return true
	}
	// Concurrency / temporary upstream error codes worth a retry.
	if strings.Contains(s, "RTM") || strings.Contains(s, "retry later") {
		return true
	}
	return false
}

// extractZAIError inspects a parsed Z.AI SSE payload for an embedded error
// (Z.AI sometimes returns HTTP 200 with the error inside the JSON body).
// Returns the human-readable detail string, or "" if no error is present.
func extractZAIError(j map[string]interface{}) string {
	if data, ok := j["data"].(map[string]interface{}); ok {
		// data.error
		if errObj, ok := data["error"].(map[string]interface{}); ok {
			detail, _ := errObj["detail"].(string)
			if detail == "" {
				if s, ok := errObj["message"].(string); ok {
					detail = s
				}
			}
			if detail != "" {
				if code, ok := errObj["code"]; ok && code != nil {
					return fmt.Sprintf("%s (code: %v)", detail, code)
				}
				return detail
			}
		}
		// data.data.error (nested variant observed in production)
		if nested, ok := data["data"].(map[string]interface{}); ok {
			if errObj, ok := nested["error"].(map[string]interface{}); ok {
				detail, _ := errObj["detail"].(string)
				if detail == "" {
					if s, ok := errObj["message"].(string); ok {
						detail = s
					}
				}
				if detail != "" {
					if code, ok := errObj["code"]; ok && code != nil {
						return fmt.Sprintf("%s (code: %v)", detail, code)
					}
					return detail
				}
			}
		}
	}
	// Top-level error (non-Z.AI shape, just in case)
	if errObj, ok := j["error"].(map[string]interface{}); ok {
		detail, _ := errObj["detail"].(string)
		if detail == "" {
			if s, ok := errObj["message"].(string); ok {
				detail = s
			}
		}
		if detail != "" {
			return detail
		}
	}
	return ""
}

// statusFromError maps a Z.AI/bridge error string to an HTTP status code.
func statusFromError(errMsg string) int {
	switch {
	case strings.Contains(errMsg, "hourly pacing cap exceeded"):
		// Pacing damper (WAF relief): UPSTREAM_HOURLY_CAP reached -> 429.
		return 429
	case strings.Contains(errMsg, "temporarily blocked this server's IP"):
		// Issue #41: IP-level WAF block — not the client's request shape.
		// 503 (retry later), not 500.
		return 503
	case strings.Contains(errMsg, "401"):
		return 401
	case strings.Contains(errMsg, "403"):
		return 403
	case strings.Contains(errMsg, "429"):
		return 429
	case strings.Contains(errMsg, "400"):
		return 400
	default:
		return 500
	}
}

// utf16IndexToByteIndex converts a UTF-16 code-unit offset — the indexing
// semantics of JavaScript strings, used by the official Z.AI web frontend
// (`content.substring(0, edit_index)`, see the prod-fe bundle) — into a
// byte offset within s. It clamps at the end of s and never returns an
// offset inside a multi-byte rune: an offset landing between the two
// units of a surrogate pair is clamped to the start of that rune.
func utf16IndexToByteIndex(s string, utf16Idx int) int {
	if utf16Idx <= 0 {
		return 0
	}
	byteIdx, units := 0, 0
	for byteIdx < len(s) {
		if units == utf16Idx {
			return byteIdx
		}
		r, size := utf8.DecodeRuneInString(s[byteIdx:])
		ru := utf16.RuneLen(r)
		if ru < 0 {
			ru = 1 // invalid byte: the JS frontend also sees one unit here
		}
		if units+ru > utf16Idx {
			return byteIdx // inside a surrogate pair — clamp to rune start
		}
		units += ru
		byteIdx += size
	}
	return len(s)
}

// commonPrefixLen returns the byte length of the longest common prefix of
// a and b. The result is always on a rune boundary, so slicing either
// string at that offset cannot produce invalid UTF-8.
func commonPrefixLen(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) {
		ra, sa := utf8.DecodeRuneInString(a[i:])
		rb, _ := utf8.DecodeRuneInString(b[i:])
		if ra != rb {
			break
		}
		i += sa
	}
	return i
}

// holdBackTail trims up to n runes from the end of s (rune-safe).
func holdBackTail(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	i, count := len(s), 0
	for i > 0 && count < n {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
		count++
	}
	return s[:i]
}

// holdBackPartialDetailsTag trims a trailing fragment that could still be
// the beginning of a <details> tag whose completion has not arrived yet,
// so a tag streamed character by character never leaks to the client.
// A COMPLETE "</details>" literal is kept (legitimate text); a complete
// "<details" is held (waiting for its ">" to decide whether it is a tag).
func holdBackPartialDetailsTag(s string) string {
	i := strings.LastIndex(s, "<")
	if i < 0 {
		return s
	}
	suffix := s[i:]
	if (len(suffix) <= len("<details") && strings.HasPrefix("<details", suffix)) ||
		(len(suffix) < len("</details>") && strings.HasPrefix("</details>", suffix)) {
		// A newline DIRECTLY before the pending tag belongs to the
		// tag's line: while the tag is incomplete it reads as body
		// text, but once the tag completes it turns structural (or
		// is trimmed). Emitting it would leave a residue the
		// append-only emitter can never retract, so hold it with
		// the tag.
		if strings.HasSuffix(s[:i], "\n") {
			return s[:i-1]
		}
		return s[:i]
	}
	return s
}

// holdBackPartialQuoteMarker trims a trailing ">" that forms the entire
// last line of s. Lines inside a <details> reasoning body are markdown-
// quoted ("> ..."), and while the body streams in one character at a
// time a new line's quote marker is transiently present as a bare ">"
// — which stripDetailsTags cannot strip yet (TrimPrefix needs the
// space) but strips one character later when the "> " completes.
// Forwarding that transient ">" makes the stripped-reasoning snapshot
// sequence non-monotonic, diverging the reasoning emitter: every later
// snapshot re-emits everything after the stale ">" — the growing-prefix
// reasoning_content duplication seen by clients. Holding the marker
// back keeps the sequence monotonic; the final flush releases it if
// the text really ends there.
func holdBackPartialQuoteMarker(s string) string {
	if !strings.HasSuffix(s, ">") {
		return s
	}
	body := s[:len(s)-1]
	if body == "" || strings.HasSuffix(body, "\n") {
		return body
	}
	return s
}

// sseEmitter forwards snapshots of a growing (and occasionally rewritten)
// text to an append-only consumer as rune-safe deltas. It tracks exactly
// what the consumer has received so far and never emits a slice that
// starts inside a multi-byte rune, so the consumer can never receive
// invalid UTF-8 (which its JSON renderer would show as U+FFFD
// replacement garble — the symptom reported in issue #23).
type sseEmitter struct {
	clientView string // exactly what the consumer has received so far
}

// delta returns the text to append to the consumer so it converges on
// target as closely as possible, and updates the tracked view:
//   - target extends the view   -> the new suffix (normal growth)
//   - target is a prefix of the view (a deep edit truncated the text)
//     -> "" — nothing can be taken back; the view is kept as-is so the
//     following growth is not re-sent from a rewound base
//   - target rewrote part of the view -> everything after the longest
//     common prefix; the stale fragment in between stays on the consumer
//     (unavoidable for append-only SSE, but it remains valid UTF-8).
//     The tracked state then re-syncs to target so later growth emits
//     only the genuinely new suffix; keeping the stale fragment in the
//     tracked view would make every later snapshot diverge at the same
//     point and re-emit everything after it on every call (cascading
//     growing-prefix duplication).
func (e *sseEmitter) delta(target string) string {
	if target == e.clientView {
		return ""
	}
	if strings.HasPrefix(target, e.clientView) {
		delta := target[len(e.clientView):]
		e.clientView = target
		return delta
	}
	cp := commonPrefixLen(e.clientView, target)
	if cp == len(target) {
		return "" // consumer already has everything target contains
	}
	delta := target[cp:]
	e.clientView = target
	return delta
}

// indexStructuralClose returns the byte index of the first close tag in s
// that does NOT sit on a ">"-quoted line, or -1. z.ai quotes every
// thinking-body line with "> " and the structural close always sits on its
// own line — but the thinking may MENTION the tags it plans to output
// (drafting the preset's hidden summary block), and those mentions always
// live on quoted lines. Matching the first bare occurrence closed the
// reasoning block early and dumped the remaining quoted thinking into the
// content channel — the 13 Oct CoT leak (traces 5f845a5c / 0b470aef).
func indexStructuralClose(s string) int {
	from := 0
	for {
		i := strings.Index(s[from:], "</details>")
		if i < 0 {
			return -1
		}
		i += from
		lineStart := 0
		if j := strings.LastIndexByte(s[:i], '\n'); j >= 0 {
			lineStart = j + 1
		}
		if !strings.HasPrefix(s[lineStart:], ">") {
			return i
		}
		from = i + 1
	}
}

// splitDetails extracts every complete <details ...>...</details> block
// from raw: the block bodies (concatenated) become reasoning, everything
// else becomes content. A trailing opener whose '>' has not arrived yet is
// held pending (neither reasoning nor content) until more data arrives.
//
// A close tag on a ">"-quoted line is a MENTION inside the reasoning body
// (the model drafting the tags it plans to output), not a block boundary —
// see indexStructuralClose. Matching the first bare occurrence closed the
// reasoning early and dumped the remaining quoted thinking into the
// content channel — the 13 Oct CoT leak (traces 5f845a5c / 0b470aef).
//
// open reports whether the tail of raw sits inside an UNFINISHED block (an
// opener — complete or not — whose closing </details> has not arrived
// yet). While open, the reasoning body is still being written upstream and
// an edit_content event may rewrite its tail, so the caller keeps the
// streaming hold-backs. The moment every block is closed the reasoning is
// settled: it must be released in full, before the answer that follows —
// holding it until the final flush let the answer overtake the reasoning
// tail on append-only SSE clients (the reasoning-after-content split from
// issue #45).
func splitDetails(raw string) (reasoning, content string, open bool) {
	var rb, cb strings.Builder
	rest := raw
	for {
		idx := strings.Index(rest, "<details")
		if idx < 0 {
			cb.WriteString(rest)
			return rb.String(), cb.String(), false
		}
		cb.WriteString(rest[:idx])
		tagEnd := strings.Index(rest[idx:], ">")
		if tagEnd < 0 {
			return rb.String(), cb.String(), true // incomplete opener at the tail — hold pending
		}
		afterTag := rest[idx+tagEnd+1:]
		closeIdx := indexStructuralClose(afterTag)
		if closeIdx < 0 {
			rb.WriteString(afterTag) // reasoning still streaming
			return rb.String(), cb.String(), true
		}
		rb.WriteString(afterTag[:closeIdx])
		rest = afterTag[closeIdx+len("</details>"):]
	}
}

// streamSSEResponse parses Z.AI's upstream SSE stream into ZAIResult chunks.
// requestId, when non-empty, prefixes every debug log line emitted here so
// maskURLToken redacts the token query parameter (device JWT) from a
// URL before it reaches debug logs (privacy: credentials never logged).
func maskURLToken(u string) string {
	if i := strings.Index(u, "token="); i >= 0 {
		if j := strings.Index(u[i:], "&"); j >= 0 {
			return u[:i] + "token=<redacted>" + u[i+j:]
		}
		return u[:i] + "token=<redacted>"
	}
	return u
}

// concurrent streams can be attributed (issue #36).
func streamSSEResponse(body io.Reader, ch chan<- ZAIResult, requestId string) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	// logPrefix labels debug output for this request. The client-visible id
	// already exists, so the label is exactly what a log reader greps for.
	logPrefix := "[DEBUG]"
	if requestId != "" {
		logPrefix = "[DEBUG] [" + requestId + "]"
	}

	// ── Accumulated state across SSE lines ──
	var fullText strings.Builder      // raw upstream content, verbatim
	contentEmitter := &sseEmitter{}   // tracks what the client has received
	reasoningEmitter := &sseEmitter{} // same for the reasoning channel

	// stripDetailsTags removes <details ...> and </details> wrappers
	// and leading "> " markdown-quote prefixes from each line.
	stripDetailsTags := func(s string) string {
		if idx := strings.Index(s, "<details"); idx >= 0 {
			if end := strings.Index(s[idx:], ">"); end >= 0 {
				s = s[:idx] + s[idx+end+1:]
			}
		}
		s = strings.ReplaceAll(s, "</details>", "")
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			lines[i] = strings.TrimPrefix(l, "> ")
		}
		return strings.TrimSpace(strings.Join(lines, "\n"))
	}

	// emitContent forwards the part of `target` the client has not seen
	// yet. All slicing is rune-safe, so clients can never receive invalid
	// UTF-8 (which their JSON renderer would show as U+FFFD replacement
	// garble — the symptom reported in issue #23). FullText carries the
	// authoritative upstream snapshot for non-stream consumers and for
	// deep-edit re-sync detection in the handlers.
	emitContent := func(target string) {
		if delta := contentEmitter.delta(target); delta != "" {
			ch <- ZAIResult{Chunk: delta, FullText: target}
		}
	}

	flush := func(final bool) {
		raw := fullText.String()

		// Split <details ...> ... </details> into reasoning vs content.
		// reasoningOpen reports whether the reasoning block is still being
		// written upstream (an opener without its </details> yet).
		reasoning, content, reasoningOpen := splitDetails(raw)
		if reasoning != "" {
			reasoning = stripDetailsTags(reasoning)
		}

		// Emit reasoning delta (prefix-aware, rune-safe). Reasoning rides
		// the same edit-based stream as content, so while the block is
		// OPEN it gets the same protection: hold back a small tail so
		// trailing edit_content backtracks are absorbed invisibly, hold
		// back a partial </details> close tag streamed character by
		// character (splitDetails folds it into the reasoning body until
		// it completes, so forwarding it would leak the fragment and
		// then rewind the snapshot), and hold back a partially-streamed
		// "> " quote marker that a later character would strip again
		// (non-monotonic snapshots diverge the emitter and duplicate
		// everything after them). Once the block CLOSES — or at the final
		// flush — the reasoning is settled and everything is released:
		// the answer that follows must never overtake the reasoning tail
		// on an append-only SSE client (issue #45).
		if !final && reasoningOpen {
			reasoning = holdBackTail(reasoning, config.StreamHoldback)
			reasoning = holdBackPartialDetailsTag(reasoning)
			reasoning = holdBackPartialQuoteMarker(reasoning)
		}
		if delta := reasoningEmitter.delta(reasoning); delta != "" {
			ch <- ZAIResult{Reasoning: delta}
		}

		// While the stream is live, keep a small tail pending so ordinary
		// trailing edit_content backtracks are absorbed invisibly, and
		// never forward a fragment that could still grow into a <details>
		// tag. The final flush releases everything.
		target := content
		if !final {
			target = holdBackTail(target, config.StreamHoldback)
			target = holdBackPartialDetailsTag(target)
		}
		emitContent(target)
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if config.Logging.Level == "debug" && trimmed != "" {
			// PRIVACY (29 Okt, owner directive): SSE payloads carry the user chat
			// text (delta_content/edit_content) - log metadata only.
			if strings.Contains(trimmed, "delta_content") || strings.Contains(trimmed, "edit_content") {
				log.Printf("%s Z.AI SSE line: <content elided, %d bytes>", logPrefix, len(trimmed))
			} else {
				log.Println(logPrefix, "Z.AI SSE line:", trimmed)
			}
		}

		if !strings.HasPrefix(trimmed, "data: ") {
			continue
		}
		dataStr := trimmed[6:]
		if dataStr == "[DONE]" {
			flush(true)
			return nil
		}

		var j map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &j); err != nil {
			if config.Logging.Level == "debug" {
				log.Println(logPrefix, "Z.AI failed to parse SSE:", dataStr)
			}
			continue
		}

		// ── Detect inline errors (HTTP 200 with error in body) ──
		if errDetail := extractZAIError(j); errDetail != "" {
			if config.Logging.Level == "debug" {
				log.Println(logPrefix, "Z.AI inline SSE error:", errDetail)
			}
			return fmt.Errorf("Z.AI error: %s", errDetail)
		}

		if data, ok := j["data"].(map[string]interface{}); ok {
			if phase, ok := data["phase"].(string); ok && phase == "done" {
				flush(true)
				return nil
			}
		}

		// ── Content accumulation ──
		// Semantics mirror the official Z.AI web frontend (prod-fe bundle):
		//   edit_content:  content = content.substring(0, edit_index) + edit_content
		//                  where edit_index is a UTF-16 code-unit offset
		//                  (JavaScript string indexing); a missing
		//                  edit_index defaults to 0 (full replacement)
		//   content:       full replacement of the accumulated text
		//   delta_content: plain append
		if data, ok := j["data"].(map[string]interface{}); ok {
			if ec, ok := data["edit_content"].(string); ok && ec != "" {
				editIndex := 0
				if ei, ok := data["edit_index"].(float64); ok {
					editIndex = int(ei)
				}
				current := fullText.String()
				byteIdx := utf16IndexToByteIndex(current, editIndex)
				fullText.Reset()
				fullText.WriteString(current[:byteIdx] + ec)
			} else if tc, ok := data["content"].(string); ok && tc != "" {
				fullText.Reset()
				fullText.WriteString(tc)
			} else if dc, ok := data["delta_content"].(string); ok && dc != "" {
				fullText.WriteString(dc)
			}
		}

		flush(false)
	}

	flush(true)
	return scanner.Err()
}
