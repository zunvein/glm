// Aliyun captcha machinery: device-token database (tokens.sqlite), the
// captcha param generator, and the agent-mode captcha cache.

package zbridge

import (
    "io"
    "os"
    "os/exec"
    "sort"
    "strings"
    "bytes"
    "compress/zlib"
    "database/sql"
    "encoding/json"
    "errors"
    "fmt"
    "log"
    "net/http"
    "sync"
    "time"
)

// ============================================================================
// DATABASE — hot-swappable holder lives in db.go; the helpers below are the
// only call sites the captcha path needs.
// ============================================================================

func getNextToken() (string, bool) {
    var token string
    err := withTokenDB(func(h dbHandle) error {
        if _, err := os.Stat(h.path); err != nil {
            logError("Database file not found: " + h.path)
            return err
        }
        return h.db.QueryRow("SELECT token FROM tokens ORDER BY id LIMIT 1;").Scan(&token)
    })
    if err != nil {
        if errors.Is(err, sql.ErrNoRows) {
            logError("No device tokens available in table 'tokens'")
        } else if err == ErrDBUnavailable {
            logError(err.Error())
        } else {
            logError("Failed to query token: " + err.Error())
        }
        return "", false
    }
    return token, true
}

func removeToken(token string) {
    err := withTokenDB(func(h dbHandle) error {
        _, err := h.db.Exec("DELETE FROM tokens WHERE token = ?;", token)
        return err
    })
    if err != nil {
        logError("Failed to delete consumed token: " + err.Error())
    }
}

// ============================================================================
// ALIYUN SIGNATURE
// ============================================================================

func generateSignature(params map[string]string, secKey string) string {
    keys := make([]string, 0, len(params)+1)
    for k := range params {
        keys = append(keys, k)
    }
    sort.Strings(keys)

    var canonical strings.Builder
    canonical.Grow(512)
    for i, k := range keys {
        if i > 0 {
            canonical.WriteByte('&')
        }
        canonical.WriteString(urlEncode(k, ""))
        canonical.WriteByte('=')
        canonical.WriteString(urlEncode(params[k], ""))
    }

    stringToSign := "POST&" + urlEncode("/", "") + "&" + urlEncode(canonical.String(), "")
    signingKey := secKey + "&"
    return base64Encode(hmacSHA1([]byte(signingKey), []byte(stringToSign)))
}

func buildQueryString(params map[string]string) string {
    keys := make([]string, 0, len(params))
    for k := range params {
        keys = append(keys, k)
    }
    sort.Strings(keys)

    var b strings.Builder
    b.Grow(512)
    for i, k := range keys {
        if i > 0 {
            b.WriteByte('&')
        }
        b.WriteString(urlEncode(k, ""))
        b.WriteByte('=')
        b.WriteString(urlEncode(params[k], ""))
    }
    return b.String()
}

// ============================================================================
// HTTP POST — pooled buffer for response, connection reuse
// ============================================================================

func httpPost(targetURL, body string, extraHeaders map[string]string) (string, error) {
    req, err := http.NewRequest("POST", targetURL, strings.NewReader(body))
    if err != nil {
        return "", err
    }
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
    req.ContentLength = int64(len(body))
    for k, v := range extraHeaders {
        req.Header.Set(k, v)
    }

    resp, err := aliyunHTTPClient.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    buf := bufPool.Get().(*bytes.Buffer)
    buf.Reset()
    if _, err := io.Copy(buf, resp.Body); err != nil {
        bufPool.Put(buf)
        return "", err
    }
    result := buf.String()
    bufPool.Put(buf)
    return result, nil
}

// ============================================================================
// CAPTCHA GENERATION — PART 1: InitCaptchaV3
// ============================================================================

func initCaptcha() (string, error) {
    params := map[string]string{
        "AccessKeyId":      accessKey,
        "Action":           "InitCaptchaV3",
        "Format":           "JSON",
        "Language":         "en",
        "Mode":             "popup",
        "SceneId":          sceneID,
        "SignatureMethod":  "HMAC-SHA1",
        "SignatureNonce":   generateUUID(),
        "SignatureVersion": "1.0",
        "Timestamp":        getTimestampUTC(),
        "UpLang":           "true",
        "Version":          "2023-03-05",
    }
    params["Signature"] = generateSignature(params, secretKey)

    body := buildQueryString(params)
    resp, err := httpPost(
        "https://no8xfe.captcha-open-southeast.aliyuncs.com/", body, nil)
    if err != nil {
        return "", err
    }

    var result InitCaptchaResponse
    if err := json.Unmarshal([]byte(resp), &result); err != nil {
        return "", fmt.Errorf("parse InitCaptchaV3 response: %w", err)
    }
    return result.CertifyID, nil
}

// ============================================================================
// CAPTCHA GENERATION — PART 2: Generate arg (RC4-like stream cipher)
// ============================================================================

var argPermTable = [64]int{
    32, 50, 10, 51, 6, 44, 37, 16, 46, 11, 62, 19, 43, 25, 23, 30,
    60, 33, 53, 34, 7, 26, 12, 48, 5, 2, 20, 4, 61, 13, 47, 49,
    18, 29, 27, 22, 1, 17, 39, 56, 41, 38, 55, 31, 15, 58, 52, 40,
    8, 57, 45, 35, 59, 36, 42, 54, 63, 3, 24, 28, 14, 9, 0, 21,
}

const argConstant = "4xrihv8zb8tf1mfj"

func generateArg(certifyID string) string {
    encoded := urlEncode(certifyID, "")

    // URL-decode (identity for already-decoded strings, kept for faithfulness)
    o := make([]byte, 0, len(encoded))
    for i := 0; i < len(encoded); {
        if encoded[i] == '%' && i+2 < len(encoded) {
            o = append(o, fromHex(encoded[i+1])<<4|fromHex(encoded[i+2]))
            i += 3
        } else {
            o = append(o, encoded[i])
            i++
        }
    }

    // KSA
    r := argPermTable
    n := argConstant
    rlen := 64

    i, j := 0, 0
    for i < rlen {
        j = (((i + j + r[i] + r[j]) >> 1) + int(n[i%len(n)])) & (rlen - 1)
        if i != j {
            r[i], r[j] = r[j], r[i]
        }
        i++
    }

    // PRGA
    t := make([]byte, 0, len(o))
    e, a := 0, 0
    for idx := 0; idx < len(o); idx++ {
        a = ((e ^ a) + (r[e] ^ r[a])) & (rlen - 1)
        if e != a {
            r[e], r[a] = r[a], r[e]
        }
        m := int(o[idx])
        m = m + e + r[e] - a - r[a]
        m = m ^ (r[e] + r[a])
        m = m ^ r[(r[e]+r[a])&(rlen-1)]
        m = m & 255
        t = append(t, byte(m))
        e = (e + 1) & (rlen - 1)
    }
    return base64Encode(t)
}

// ============================================================================
// CAPTCHA GENERATION — PART 4: ali_hash (custom hash with 16-byte state)
// ============================================================================

func aliHash(inputStr, saltStr string) string {
    o := inputStr
    r := saltStr
    aLen := len(o)
    m := len(r)

    var e [16]int
    for i := 0; i < 16; i++ {
        e[i] = (i << 4) + (i % 16)
    }
    f := 16

    i, j := 0, 0
    for i < f {
        j = (((i + j + e[i] + e[j]) >> 1) + int(r[i%m])) & (f - 1)
        e[i], e[j] = e[j], e[i]
        i++
    }

    idx, p, q := 0, 0, 0
    for idx < aLen {
        q = ((p ^ q) + (e[p] ^ e[q])) & (f - 1)
        e[p], e[q] = e[q], e[p]
        C := int(o[idx])
        C = (C + p + q) ^ e[p] ^ e[q]
        C = C & 255
        e[p] = C
        p = (p + 1) & (f - 1)
        idx++
    }

    for step := 0; step < 2*f; step++ {
        pos := step % f
        if pos != 0 {
            e[pos] ^= e[pos-1]
        } else {
            e[0] ^= e[f-1]
        }
    }

    var result [32]byte
    for i, b := range e {
        result[i*2] = hexLower[(b>>4)&0xF]
        result[i*2+1] = hexLower[b&0xF]
    }
    return string(result[:])
}

// ============================================================================
// CAPTCHA GENERATION — PART 7: encrypt (same RC4-like cipher, different key)
// ============================================================================

const encryptKey = "3e627e1b4c63f913"

func encrypt(plaintext []byte) string {
    o := plaintext
    n := encryptKey
    r := argPermTable
    rlen := 64

    oKsa, tKsa := 0, 0
    for oKsa < rlen {
        tKsa = (((oKsa + tKsa + r[oKsa] + r[tKsa]) >> 1) + int(n[oKsa%len(n)])) & (rlen - 1)
        if oKsa != tKsa {
            r[oKsa], r[tKsa] = r[tKsa], r[oKsa]
        }
        oKsa++
    }

    t := make([]byte, 0, len(o))
    e, a := 0, 0
    for nPrga := 0; nPrga < len(o); nPrga++ {
        a = ((e ^ a) + (r[e] ^ r[a])) & (rlen - 1)
        if e != a {
            r[e], r[a] = r[a], r[e]
        }
        m := int(o[nPrga])
        m = m + e + r[e] - a - r[a]
        m = m ^ (r[e] + r[a])
        m = m ^ r[(r[e]+r[a])&(rlen-1)]
        m = m & 255
        t = append(t, byte(m))
        e = (e + 1) & (rlen - 1)
    }
    return base64Encode(t)
}

// ============================================================================
// ZLIB COMPRESS — pooled writer, pooled output buffer
// ============================================================================

func zlibCompress(data []byte) []byte {
    buf := bufPool.Get().(*bytes.Buffer)
    buf.Reset()
    buf.Grow(len(data) + len(data)/2 + 128)

    w := zlibWriterPool.Get().(*zlib.Writer)
    w.Reset(buf)
    w.Write(data)
    w.Close()
    zlibWriterPool.Put(w)

    result := make([]byte, buf.Len())
    copy(result, buf.Bytes())
    bufPool.Put(buf)
    return result
}

// ============================================================================
// CAPTCHA GENERATION — PART 8: VerifyCaptchaV3
// ============================================================================

func verifyCaptcha(certifyID, dataValue, deviceToken string) (string, error) {
    cvpJSON, err := jsonMarshal(CVP{
        CertifyID:   certifyID,
        Data:        dataValue,
        DeviceToken: deviceToken,
        SceneID:     sceneID,
    })
    if err != nil {
        return "", err
    }

    params := map[string]string{
        "AccessKeyId":        accessKey,
        "Action":             "VerifyCaptchaV3",
        "Format":             "JSON",
        "SignatureMethod":    "HMAC-SHA1",
        "SignatureVersion":   "1.0",
        "Timestamp":          getTimestampUTC(),
        "Version":            "2023-03-05",
        "SceneId":            sceneID,
        "CertifyId":          certifyID,
        "CaptchaVerifyParam": string(cvpJSON),
        "SignatureNonce":     generateUUID(),
    }
    params["Signature"] = generateSignature(params, secretKey)

    body := buildQueryString(params)
    resp, err := httpPost(
        "https://no8xfe-verify.captcha-open-southeast.aliyuncs.com/",
        body, map[string]string{"Referer": ""})
    if err != nil {
        return "", err
    }

    var respJSON VerifyCaptchaResponse
    if err := json.Unmarshal([]byte(resp), &respJSON); err != nil {
        return "", fmt.Errorf("parse VerifyCaptchaV3 response: %w", err)
    }

    if respJSON.Success && respJSON.Result.VerifyResult {
        st := respJSON.Result.SecurityToken
        ci := respJSON.Result.CertifyID
        if st != "" && ci != "" {
            fpJSON, err := jsonMarshal(FinalPayload{
                CertifyID:     ci,
                IsSign:        true,
                SceneID:       sceneID,
                SecurityToken: st,
            })
            if err != nil {
                return "", err
            }
            return base64Encode(fpJSON), nil
        }
        logError("VerifyCaptchaV3 succeeded but securityToken/certifyId empty for deviceToken=" + deviceToken)
    } else if respJSON.Success {
        logError("deviceToken failed verification (VerifyResult=false): " + deviceToken)
    } else {
        logError("VerifyCaptchaV3 request unsuccessful for deviceToken=" + deviceToken + " response=" + resp)
    }
    return "", nil
}

// ============================================================================
// COMPUTE FINAL PAYLOAD — tries tokens until success or exhausted
// ============================================================================

// CAPTCHA CIRCUIT BREAKER — during an upstream verify-storm every compute
// burns up to maxTokenRetries tokens and the background cache loop keeps
// re-spawning generates without pause, so a fresh 120-token pool evaporates
// in ~2-3 minutes (30 Okt incident #6). After a few consecutive failed
// computes the breaker opens: computeFinalPayload returns "" immediately
// WITHOUT consuming tokens until the cooldown elapses. A fresh pool swap
// (POST /sqlite from refill/watchdog) resets it so recovery is immediate.

const captchaBreakerThreshold = 3
const captchaBreakerCooldown = 90 * time.Second

var (
    captchaBreakerMu       sync.Mutex
    captchaComputeFailures int
    captchaBreakerOpenAt   time.Time // zero = closed
)

func captchaBreakerOpen() bool {
    captchaBreakerMu.Lock()
    defer captchaBreakerMu.Unlock()
    return time.Now().Before(captchaBreakerOpenAt)
}

var (
    // stormRefillMu serialises auto-reharvest so a storm never triggers two
    // overlapping refill.sh browser visits. refill.sh hot-swaps a fresh pool
    // via POST /sqlite, which resets the breaker (swapDB -> resetCaptchaBreaker),
    // so one refill during a storm is enough to unblock the next compute.
    stormRefillMu       sync.Mutex
    stormRefillLast     time.Time // last time we auto-reharvested, to rate-limit
    stormRefillMinGap   = 90 * time.Second
    stormRefillScript   = "pool/refill.sh"
)

// spawnStormRefill fires refill.sh in a background goroutine so the request
// path is never blocked by the (browser-visit) harvest. Single-flight: if a
// refill already ran within stormRefillMinGap, this is a no-op. The freshly
// minted pool is swapped in by refill.sh itself, and the swap resets the
// breaker, so the next compute gets fresh device identities that Aliyun has
// not yet flagged.
func spawnStormRefill() {
    stormRefillMu.Lock()
    if time.Since(stormRefillLast) < stormRefillMinGap {
        stormRefillMu.Unlock()
        return
    }
    stormRefillLast = time.Now()
    stormRefillMu.Unlock()

    log.Printf("[Captcha] storm detected — auto-refilling fresh device-token pool (refill.sh)")
    go func() {
        cmd := exec.Command("/bin/sh", stormRefillScript)
        cmd.Dir = "/home/ec2-user/zai-api-dev"
        cmd.Env = append(os.Environ(), "DISPLAY=:1", "HOME=/home/ec2-user")
        out, err := cmd.CombinedOutput()
        if err != nil {
            logError(fmt.Sprintf("[Captcha] storm auto-refill FAILED: %v out=%s", err, string(out)))
            return
        }
        log.Printf("[Captcha] storm auto-refill completed: %s", strings.TrimSpace(string(out)))
    }()
}

func recordCaptchaFailure() {
    captchaBreakerMu.Lock()
    captchaComputeFailures++
    // Re-arm on every storm burst: openAt stays in the past after a cooldown
    // expires, so "IsZero only" would let the breaker fire exactly once and
    // leave the following cycle unbounded (observed live 30 Okt: pool
    // 120 -> 18 in 4 min during one storm).
    tripped := false
    if captchaComputeFailures >= captchaBreakerThreshold && !time.Now().Before(captchaBreakerOpenAt) {
        captchaBreakerOpenAt = time.Now().Add(captchaBreakerCooldown)
        captchaComputeFailures = 0
        tripped = true
        log.Printf("[Captcha] circuit breaker OPEN for %s after %d failed computes (token burn capped)",
            captchaBreakerCooldown, captchaBreakerThreshold)
    }
    captchaBreakerMu.Unlock()

    // The breaker tripped => a verify-storm is live and the current pool's
    // device identities are all being flagged by Aliyun. The fastest native
    // recovery (no headful browser daemon, no RAM cost) is to harvest a fresh
    // batch and hot-swap: fresh identities are accepted until Aliyun re-flags
    // them ~10-30 min later. Trigger it exactly once per trip (single-flight
    // + rate-limited above).
    if tripped {
        spawnStormRefill()
    }
}

func recordCaptchaSuccess() {
    captchaBreakerMu.Lock()
    defer captchaBreakerMu.Unlock()
    if !captchaBreakerOpenAt.IsZero() {
        log.Printf("[Captcha] circuit breaker CLOSED - captcha compute succeeded")
    }
    captchaComputeFailures = 0
    captchaBreakerOpenAt = time.Time{}
}

func resetCaptchaBreaker() {
    captchaBreakerMu.Lock()
    defer captchaBreakerMu.Unlock()
    captchaComputeFailures = 0
    captchaBreakerOpenAt = time.Time{}
}

func computeFinalPayload() string {
    if captchaBreakerOpen() {
        log.Printf("[Captcha] circuit breaker open - fail fast without consuming tokens")
        return ""
    }
    for attempt := 0; attempt < maxTokenRetries; attempt++ {
        deviceToken, ok := getNextToken()
        if !ok {
            logError(fmt.Sprintf("No device tokens remaining (attempt %d/%d)",
                attempt+1, maxTokenRetries))
            recordCaptchaFailure()
            return ""
        }
        logInfo(fmt.Sprintf("Attempt %d/%d using deviceToken=%s",
            attempt+1, maxTokenRetries, deviceToken))

        payload, err := tryCompute(deviceToken)
        if err != nil {
            logError(fmt.Sprintf("Attempt %d failed for deviceToken=%s: %v",
                attempt+1, deviceToken, err))
            continue
        }
        if payload != "" {
            recordCaptchaSuccess()
            return payload
        }
        logError("deviceToken=" + deviceToken + " produced empty payload, retrying")
    }
    logError(fmt.Sprintf("All %d token retries exhausted", maxTokenRetries))
    recordCaptchaFailure()
    return ""
}

func tryCompute(deviceToken string) (string, error) {
    certifyID, err := initCaptcha()
    if err != nil {
        removeToken(deviceToken)
        return "", fmt.Errorf("initCaptcha: %w", err)
    }

    argValue := generateArg(certifyID)
    ct := currentTimeMillis()

    track := Track{
        TrackList: TrackList{
            StartTime: ct,
        },
        TrackStartTime: ct,
        VerifyTime:     ct + 300,
        Arg:            argValue,
    }
    jsonBytes, err := jsonMarshal(track)
    if err != nil {
        removeToken(deviceToken)
        return "", err
    }

    h := aliHash(string(jsonBytes), "0000")
    combined := h + string(jsonBytes)
    compressed := zlibCompress([]byte(combined))
    fb64 := base64Encode(compressed)
    finalVal := encrypt([]byte(fb64))

    // Always remove token after use — prevents conflicts
    removeToken(deviceToken)

    payload, err := verifyCaptcha(certifyID, finalVal, deviceToken)
    if err != nil {
        return "", fmt.Errorf("verifyCaptcha: %w", err)
    }
    return payload, nil
}

// ============================================================================
// CAPTCHA CACHE — Background async generation for speed
// ============================================================================

type cachedCaptcha struct {
    value       string
    generatedAt time.Time
}

type CaptchaCache struct {
    mu         sync.Mutex
    params     []cachedCaptcha
    maxParams  int
    generating int
    active     bool
    lastActive time.Time
}

var captchaCache = &CaptchaCache{
    maxParams:  2,
    lastActive: time.Now(),
}

func (c *CaptchaCache) markActive() {
    c.mu.Lock()
    c.lastActive = time.Now()
    c.active = true
    c.mu.Unlock()
}

func (c *CaptchaCache) Get() (string, bool) {
    c.markActive()
    c.mu.Lock()
    defer c.mu.Unlock()

    // Sweep expired (75s TTL)
    var valid []cachedCaptcha
    for _, p := range c.params {
        if time.Since(p.generatedAt) < 75*time.Second {
            valid = append(valid, p)
        }
    }
    c.params = valid

    if len(c.params) > 0 {
        val := c.params[0].value
        c.params = c.params[1:]
        return val, true
    }
    return "", false
}

func (c *CaptchaCache) Run() {
    // Wait a moment before starting to allow session to init
    time.Sleep(2 * time.Second)
    ticker := time.NewTicker(500 * time.Millisecond)
    defer ticker.Stop()

    for range ticker.C {
        c.mu.Lock()
        
        // If no activity in the last 3 minutes, pause generation to save tokens
        if time.Since(c.lastActive) > 3*time.Minute {
            c.active = false
            c.mu.Unlock()
            continue
        }
        c.active = true

        // Sweep expired
        var valid []cachedCaptcha
        for _, p := range c.params {
            if time.Since(p.generatedAt) < 75*time.Second {
                valid = append(valid, p)
            }
        }
        c.params = valid

        needed := c.maxParams - len(c.params) - c.generating
        if needed > 0 {
            c.generating += needed
            c.mu.Unlock()
            // Launch generation in parallel
            for i := 0; i < needed; i++ {
                go c.generate()
            }
        } else {
            c.mu.Unlock()
        }
    }
}

func (c *CaptchaCache) generate() {
    startedAt := time.Now()
    payload := computeFinalPayload()
    
    c.mu.Lock()
    c.generating--
    if payload != "" {
        c.params = append(c.params, cachedCaptcha{
            value:       payload,
            generatedAt: time.Now(),
        })
        logInfo(fmt.Sprintf("[Captcha Cache] ✓ generated param in %.1fs (cache size: %d)", time.Since(startedAt).Seconds(), len(c.params)))
    } else {
        logError("[Captcha Cache] ✗ failed to generate param")
    }
    c.mu.Unlock()
}

// ============================================================================
// CAPTCHA VERIFICATION PARAM — IN-MEMORY (no FIFO / named pipe)
// ============================================================================

// captchaParamOverride, when non-empty, is returned by getCaptchaVerifyParam
// instead of hitting the real Aliyun machinery. It is a test seam in the
// same spirit as wafProbeTransport (waf.go): whitebox tests set it to
// drive sendToZAI against a mock upstream without agent mode (the cache
// path is agent-mode only).
//
// As of 31 Okt it is ALSO a production path: the headful captcha daemon
// (tools-28okt/zcapd) captures a REAL AliyunSDK captcha_verify_param from a
// live browser during verify-storms (when the programmatic VerifyCaptchaV3
// call is rejected as "empty payload") and POSTs it here via /captcha. The
// param is validated (must be non-empty base64) and has a TTL so a stale
// browser-bound param never lingers. Thread-safe via a mutex (the compute
// path and the HTTP handler may race).
var (
    captchaParamOverrideMu sync.Mutex
    captchaParamOverride   string
    captchaParamOverrideAt time.Time
)

// captchaOverrideTTL bounds how long a daemon-injected param is trusted.
// The param is single-use at Z.AI (each captcha_verify_param is consumed on
// the first completion), so a fresh one must be fed continuously; a stale
// one from a crashed daemon must expire instead of serving every request
// with a dead param.
const captchaOverrideTTL = 60 * time.Second

// setCaptchaParamOverride stores a daemon-supplied headful captcha param and
// re-arms the breaker (a fresh param means the storm may have cleared — let
// compute try again immediately instead of waiting out the cooldown).
func setCaptchaParamOverride(val string) bool {
    if val == "" {
        return false
    }
    captchaParamOverrideMu.Lock()
    defer captchaParamOverrideMu.Unlock()
    captchaParamOverride = val
    captchaParamOverrideAt = time.Now()
    resetCaptchaBreaker()
    return true
}

// clearCaptchaParamOverride drops an injected param (used on /sqlite swap so
// a new pool batch starts clean).
func clearCaptchaParamOverride() {
    captchaParamOverrideMu.Lock()
    defer captchaParamOverrideMu.Unlock()
    captchaParamOverride = ""
    captchaParamOverrideAt = time.Time{}
}

func getCaptchaVerifyParam() (string, error) {
    captchaParamOverrideMu.Lock()
    override := captchaParamOverride
    // A zero timestamp means the override was set directly (the whitebox /
    // blackbox test seam and older direct-assignment callers), which is
    // always fresh. A non-zero timestamp is a daemon-injected headful param
    // with a TTL. Stale daemon params are dropped and fall through to compute.
    fresh := captchaParamOverrideAt.IsZero() || time.Since(captchaParamOverrideAt) < captchaOverrideTTL
    if override != "" && !fresh {
        // Stale daemon param — drop it and fall through to compute.
        override = ""
        captchaParamOverride = ""
    }
    captchaParamOverrideMu.Unlock()
    if override != "" {
        log.Printf("[Captcha] using daemon-injected headful param (TTL active)")
        return override, nil
    }
    if config.AgentMode {
        if val, ok := captchaCache.Get(); ok {
            logInfo("[Captcha Cache] hit - using cached param")
            return val, nil
        }
        logInfo("[Captcha Cache] miss - generating synchronously")
    }

    startedAt := time.Now()
    log.Printf("[Captcha] → computing CaptchaVerifyParam (in-memory IPC)")

    type result struct {
        val string
        err error
    }
    ch := make(chan result, 1)

    go func() {
        payload := computeFinalPayload()
        if payload == "" {
            ch <- result{"", errors.New("captcha generation returned empty payload")}
            return
        }
        ch <- result{payload, nil}
    }()

    select {
    case r := <-ch:
        elapsed := time.Since(startedAt).Seconds()
        if r.err != nil {
            log.Printf("[Captcha] ✗ error: %s", r.err.Error())
            return "", r.err
        }
        if r.val == "" {
            log.Printf("[Captcha] ✗ empty response after %.1fs", elapsed)
            return "", errors.New("captcha generation returned empty response")
        }
        log.Printf("[Captcha] ✓ got %db in %.1fs", len(r.val), elapsed)
        return r.val, nil
    case <-time.After(90 * time.Second):
        elapsed := time.Since(startedAt).Seconds()
        log.Printf("[Captcha] ✗ timeout after %.1fs", elapsed)
        return "", errors.New("captcha generation timeout after 90s")
    }
}

