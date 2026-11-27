package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const quickFormatVersion = "aws-quick/v1"

type QuickAccountRecord struct {
	Format        string         `json:"format,omitempty"`
	ID            string         `json:"id,omitempty"`
	Email         string         `json:"email"`
	Label         string         `json:"label,omitempty"`
	DisplayName   string         `json:"display_name,omitempty"`
	Provider      string         `json:"provider"`
	BuildStage    string         `json:"build_stage,omitempty"`
	Region        string         `json:"region"`
	IDCRegion     string         `json:"idc_region,omitempty"`
	IssuerURL     string         `json:"issuer_url,omitempty"`
	TenantURL     string         `json:"tenant_url"`
	RedirectURI   string         `json:"redirect_uri,omitempty"`
	AWSAccountID  string         `json:"aws_account_id,omitempty"`
	AccountName   string         `json:"account_name,omitempty"`
	IDCInstanceID string         `json:"idc_instance_id,omitempty"`
	ClientID      string         `json:"client_id,omitempty"`
	ClientSecret  string         `json:"client_secret,omitempty"`
	RefreshToken  string         `json:"refresh_token"`
	AccessToken   string         `json:"access_token,omitempty"`
	IDToken       string         `json:"id_token,omitempty"`
	ExpiresIn     int64          `json:"expires_in,omitempty"`
	ObtainedAt    int64          `json:"obtained_at,omitempty"`
	Proxy         string         `json:"proxy,omitempty"`
	Enabled       *bool          `json:"enabled,omitempty"`
	UserInfo      map[string]any `json:"user_info,omitempty"`
	Discovery     map[string]any `json:"discovery,omitempty"`
}

func (s *Server) uploadAllowed(r *http.Request) bool {
	configured := strings.TrimSpace(os.Getenv("QW_UPLOAD_API_KEY"))
	token := s.tokens.tokenFromRequest(r)
	if configured != "" && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(configured)) == 1 {
		return true
	}
	if s.tokens.Allowed(r) {
		return true
	}
	return false
}

func (s *Server) handleAccountsUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	if !s.uploadAllowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid proxy API key")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read upload payload"})
		return
	}
	records, err := parseQuickBundlePayload(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	summary := s.importQuickRecords(r.Context(), records)
	writeJSON(w, http.StatusOK, summary)
}

func decodeJWTClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

func pickMapStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if m == nil {
			continue
		}
		if v, ok := m[k]; ok && v != nil {
			if s, ok := v.(string); ok {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

func pickMapInt64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if m == nil {
			continue
		}
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		}
	}
	return 0
}

func parseNestedMap(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		trimmed := strings.TrimSpace(t)
		if strings.HasPrefix(trimmed, "{") {
			var out map[string]any
			if json.Unmarshal([]byte(trimmed), &out) == nil {
				return out
			}
		}
	}
	return nil
}

func parseQuickBundlePayload(raw []byte) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("upload payload is empty")
	}
	var obj map[string]any
	if err := json.Unmarshal(trimmed, &obj); err == nil && obj != nil {
		if list, ok := obj["accounts"].([]any); ok {
			out := make([]map[string]any, 0, len(list))
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					out = append(out, m)
				}
			}
			if len(out) == 0 {
				return nil, errors.New("accounts list is empty")
			}
			return out, nil
		}
		if rawStr, ok := obj["raw"].(string); ok && strings.TrimSpace(rawStr) != "" {
			return parseQuickBundlePayload([]byte(rawStr))
		}
		return []map[string]any{obj}, nil
	}

	var arr []map[string]any
	if err := json.Unmarshal(trimmed, &arr); err == nil {
		if len(arr) == 0 {
			return nil, errors.New("accounts array is empty")
		}
		return arr, nil
	}

	var records []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(trimmed))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return nil, fmt.Errorf("invalid JSONL entry: %w", err)
		}
		records = append(records, item)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errors.New("no valid AWS Quick records found")
	}
	return records, nil
}

func buildAccountFromQuickMap(item map[string]any, defaultSurface string, now int64) (Account, error) {
	merged := make(map[string]any, len(item))
	if upstreamMap := parseNestedMap(item["upstream"]); upstreamMap != nil {
		for k, v := range upstreamMap {
			merged[k] = v
		}
	}
	for k, v := range item {
		merged[k] = v
	}

	tokensMap := parseNestedMap(merged["tokens"])
	clientMap := parseNestedMap(merged["client"])
	discoveryMap := parseNestedMap(merged["discovery"])
	userInfoMap := parseNestedMap(merged["user_info"])
	sessionMap := parseNestedMap(merged["session"])

	provider := strings.ToLower(pickMapStr(merged, "provider"))
	if provider == "" {
		provider = strings.ToLower(pickMapStr(sessionMap, "provider"))
	}
	if provider == "" {
		provider = "social"
	}
	switch provider {
	case "social", "enterprise", "gateway":
	case "builderid", "builder_id":
		provider = "social"
	default:
		return Account{}, fmt.Errorf("unsupported provider %q", provider)
	}

	buildStage := pickMapStr(merged, "build_stage")
	if buildStage == "" {
		buildStage = "prod"
	}
	refreshToken := pickMapStr(merged, "refresh_token", "refreshToken")
	if refreshToken == "" {
		refreshToken = pickMapStr(tokensMap, "refresh_token", "refreshToken")
	}
	accessToken := pickMapStr(merged, "access_token", "accessToken")
	if accessToken == "" {
		accessToken = pickMapStr(tokensMap, "access_token", "accessToken")
	}
	idToken := pickMapStr(merged, "id_token", "idToken")
	if idToken == "" {
		idToken = pickMapStr(tokensMap, "id_token", "idToken")
	}
	if idToken == "" {
		idToken = pickMapStr(sessionMap, "id_token", "jwt")
	}

	clientID := pickMapStr(merged, "client_id", "clientId")
	if clientID == "" {
		clientID = pickMapStr(clientMap, "client_id", "clientId")
	}
	if clientID == "" {
		clientID = pickMapStr(discoveryMap, "client_id", "clientId")
	}
	clientSecret := pickMapStr(merged, "client_secret", "clientSecret")
	if clientSecret == "" {
		clientSecret = pickMapStr(clientMap, "client_secret", "clientSecret")
	}
	if clientSecret == "" {
		clientSecret = pickMapStr(discoveryMap, "client_secret", "clientSecret")
	}

	if refreshToken == "" && accessToken == "" && idToken == "" {
		return Account{}, errors.New("missing refresh_token or access_token")
	}
	if provider == "social" && refreshToken != "" && (clientID == "" || clientSecret == "") {
		return Account{}, errors.New("social account requires client_id and client_secret")
	}

	email := pickMapStr(merged, "email")
	if email == "" {
		email = pickMapStr(userInfoMap, "email")
	}
	claims := decodeJWTClaims(idToken)
	if email == "" {
		email = pickMapStr(claims, "email")
	}
	if email == "" {
		return Account{}, errors.New("missing email")
	}

	region := pickMapStr(merged, "region")
	if region == "" {
		region = pickMapStr(discoveryMap, "region")
	}
	if region == "" {
		region = pickMapStr(sessionMap, "region")
	}
	if region == "" {
		region = "us-east-1"
	}
	idcRegion := pickMapStr(merged, "idc_region", "idcRegion")
	if idcRegion == "" {
		idcRegion = pickMapStr(clientMap, "idc_region", "idcRegion")
	}
	if idcRegion == "" {
		idcRegion = pickMapStr(discoveryMap, "idc_region", "idcRegion")
	}
	if idcRegion == "" {
		idcRegion = region
	}
	issuerURL := pickMapStr(merged, "issuer_url", "issuerUrl")
	if issuerURL == "" {
		issuerURL = pickMapStr(clientMap, "issuer_url", "issuerUrl")
	}
	if issuerURL == "" {
		issuerURL = pickMapStr(discoveryMap, "issuer_url", "issuerUrl")
	}

	tenantURL := pickMapStr(merged, "tenant_url", "tenantUrl")
	if tenantURL == "" {
		tenantURL = pickMapStr(discoveryMap, "tenant_url", "tenantUrl")
	}
	if tenantURL == "" {
		tenantURL = pickMapStr(sessionMap, "tenant_url")
	}
	if tenantURL == "" {
		tenantURL = socialDataPlaneProd
	}
	tenantURL = strings.TrimRight(tenantURL, "/")
	if !strings.HasPrefix(tenantURL, "http://") && !strings.HasPrefix(tenantURL, "https://") {
		tenantURL = "https://" + tenantURL
	}
	parsedTenant, err := url.Parse(tenantURL)
	if err != nil || parsedTenant.Host == "" {
		return Account{}, errors.New("invalid tenant_url")
	}
	wsHost := parsedTenant.Host

	redirectURI := pickMapStr(merged, "redirect_uri", "redirectUri")
	if redirectURI == "" {
		redirectURI = "http://127.0.0.1:18080"
	}

	awsAccountID := pickMapStr(merged, "aws_account_id", "awsAccountId", "account_id")
	if awsAccountID == "" {
		awsAccountID = pickMapStr(userInfoMap, "aws_account_id")
	}
	if awsAccountID == "" {
		awsAccountID = pickMapStr(discoveryMap, "account")
	}
	if awsAccountID == "" {
		awsAccountID = pickMapStr(sessionMap, "aws_account_id")
	}
	idcInstanceID := pickMapStr(merged, "idc_instance_id", "idcInstanceId")
	if idcInstanceID == "" {
		idcInstanceID = pickMapStr(discoveryMap, "idc_instance_id")
	}
	accountName := pickMapStr(merged, "account_name", "accountName")
	if accountName == "" {
		accountName = pickMapStr(userInfoMap, "account_name")
	}
	if accountName == "" {
		accountName = pickMapStr(sessionMap, "account_name")
	}
	displayName := pickMapStr(merged, "display_name", "name")
	if displayName == "" {
		displayName = pickMapStr(userInfoMap, "display_name", "user_name")
	}
	label := pickMapStr(merged, "label", "nickname")
	if label == "" {
		label = displayName
	}

	proxyURL, err := normalizeProxyURL(pickMapStr(merged, "proxy", "proxy_url", "proxyURL"))
	if err != nil {
		return Account{}, err
	}

	enabled := true
	if v, ok := merged["enabled"].(bool); ok {
		enabled = v
	}

	expiresIn := pickMapInt64(merged, "expires_in", "expiresIn")
	if expiresIn == 0 {
		expiresIn = pickMapInt64(tokensMap, "expires_in", "expiresIn")
	}
	obtainedAt := pickMapInt64(merged, "obtained_at", "obtainedAt")
	if obtainedAt == 0 {
		obtainedAt = pickMapInt64(sessionMap, "obtained_at")
	}
	if obtainedAt == 0 {
		obtainedAt = now
	}
	usableUntil := pickMapInt64(merged, "usable_until", "usableUntil")
	if usableUntil == 0 {
		usableUntil = pickMapInt64(sessionMap, "usable_until")
	}
	if usableUntil == 0 && expiresIn > 0 {
		usableUntil = obtainedAt + expiresIn
	}
	if usableUntil == 0 {
		usableUntil = pickMapInt64(claims, "exp")
	}

	jwtToken := idToken
	if jwtToken == "" {
		jwtToken = accessToken
	}
	actorARN := pickMapStr(merged, "actor_arn")
	if actorARN == "" {
		actorARN = pickMapStr(userInfoMap, "user_arn")
	}
	if actorARN == "" {
		actorARN = pickMapStr(sessionMap, "actor_arn")
	}
	if actorARN == "" {
		sub := pickMapStr(claims, "sub")
		if strings.HasPrefix(sub, "arn:") {
			actorARN = sub
		}
	}

	surface := pickMapStr(sessionMap, "surface")
	if surface == "" {
		surface = defaultSurface
	}
	if surface == "" {
		surface = "QUICK_WORK_DESKTOP_WINDOWS"
	}

	normUserInfo := make(map[string]any, len(userInfoMap)+4)
	for k, v := range userInfoMap {
		normUserInfo[k] = v
	}
	normUserInfo["email"] = email
	if displayName != "" {
		normUserInfo["display_name"] = displayName
	}
	if awsAccountID != "" {
		normUserInfo["aws_account_id"] = awsAccountID
	}
	if accountName != "" {
		normUserInfo["account_name"] = accountName
	}

	normDiscovery := make(map[string]any, len(discoveryMap)+5)
	for k, v := range discoveryMap {
		normDiscovery[k] = v
	}
	if issuerURL != "" {
		normDiscovery["issuer_url"] = issuerURL
	}
	normDiscovery["tenant_url"] = tenantURL
	if awsAccountID != "" {
		normDiscovery["account"] = awsAccountID
	}
	if idcInstanceID != "" {
		normDiscovery["idc_instance_id"] = idcInstanceID
	}
	if idcRegion != "" {
		normDiscovery["idc_region"] = idcRegion
	}

	upstreamDoc := map[string]any{
		"version":     1,
		"provider":    provider,
		"build_stage": buildStage,
		"region":      region,
		"redirect_uri": redirectURI,
		"tenant_url":  tenantURL,
		"user_info":   normUserInfo,
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"id_token":      idToken,
			"expires_in":    expiresIn,
		},
		"client": map[string]any{
			"client_id":     clientID,
			"client_secret": clientSecret,
			"idc_region":    idcRegion,
			"issuer_url":    issuerURL,
		},
		"discovery":   normDiscovery,
		"obtained_at": obtainedAt,
	}
	upstreamBytes, err := json.MarshalIndent(upstreamDoc, "", "  ")
	if err != nil {
		return Account{}, err
	}
	upstreamBytes = append(upstreamBytes, '\n')

	sess := Session{
		JWT:         jwtToken,
		IDToken:     jwtToken,
		WSHost:      wsHost,
		WSPath:      "",
		TenantURL:   tenantURL,
		Provider:    provider,
		ActorARN:    actorARN,
		Surface:     surface,
		AccountID:   awsAccountID,
		Region:      region,
		AccountName: accountName,
		UsableUntil: usableUntil,
		ObtainedAt:  obtainedAt,
	}
	if provider == "gateway" {
		sess.WSPath = "/ws/service/SpaceNeedleConversationService/operation/ChatBidirectionalStream"
	}

	acc := Account{
		Label:       label,
		Email:       email,
		DisplayName: displayName,
		Provider:    provider,
		Region:      region,
		AWSAccount:  awsAccountID,
		Proxy:       proxyURL,
		Enabled:     enabled,
		Session:     sess,
		Upstream:    json.RawMessage(upstreamBytes),
		Quota:       quotaFromUpstream(upstreamBytes, now),
	}
	return acc, nil
}

func (s *Server) prepareImportedAccount(ctx context.Context, acc Account, now int64) (Account, error) {
	if err := acc.Session.validate(); err == nil && (acc.Session.UsableUntil == 0 || acc.Session.UsableUntil > now+60) {
		return acc, nil
	}
	if !hasRefresh(acc.Upstream) {
		return acc, acc.Session.validate()
	}
	dir, err := os.MkdirTemp("", "qwp-import-*")
	if err != nil {
		return acc, err
	}
	defer os.RemoveAll(dir)
	if err := acc.WriteWorkDir(dir); err != nil {
		return acc, err
	}
	jobCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := s.runAuthDir(jobCtx, dir, acc.Proxy, "refresh"); err != nil {
		return acc, fmt.Errorf("Quick token refresh failed for %s: %w", acc.Email, err)
	}
	fresh, err := loadLogin(dir)
	if err != nil {
		return acc, err
	}
	fresh.Label = acc.Label
	if fresh.Email == "" {
		fresh.Email = acc.Email
	}
	if fresh.DisplayName == "" {
		fresh.DisplayName = acc.DisplayName
	}
	fresh.Proxy = acc.Proxy
	fresh.Enabled = acc.Enabled
	return fresh, nil
}

func (s *Server) importQuickRecords(ctx context.Context, records []map[string]any) map[string]any {
	now := time.Now().Unix()
	results := make([]map[string]any, 0, len(records))
	var (
		createdCount int
		updatedCount int
		failedCount  int
		errorsList   []string
	)
	for i, item := range records {
		acc, err := buildAccountFromQuickMap(item, s.cfg.Surface, now)
		if err != nil {
			failedCount++
			email := pickMapStr(item, "email")
			if email == "" {
				email = fmt.Sprintf("#%d", i+1)
			}
			msg := fmt.Sprintf("%s: %s", email, err.Error())
			errorsList = append(errorsList, msg)
			results = append(results, map[string]any{"email": email, "status": "failed", "error": err.Error()})
			continue
		}
		prepared, err := s.prepareImportedAccount(ctx, acc, now)
		if err != nil {
			failedCount++
			msg := fmt.Sprintf("%s: %s", acc.Email, err.Error())
			errorsList = append(errorsList, msg)
			results = append(results, map[string]any{"email": acc.Email, "status": "failed", "error": err.Error()})
			continue
		}
		saved, isCreated, err := s.accounts.UpsertImported(prepared)
		if err != nil {
			failedCount++
			msg := fmt.Sprintf("%s: %s", acc.Email, err.Error())
			errorsList = append(errorsList, msg)
			results = append(results, map[string]any{"email": acc.Email, "status": "failed", "error": err.Error()})
			continue
		}
		if saved.Quota == nil && accountAccessToken(saved) != "" {
			if q, qErr := fetchAccountQuota(ctx, saved); qErr == nil && q != nil {
				_ = s.accounts.UpdateQuota(saved.ID, *q)
			}
		}
		if isCreated {
			createdCount++
		} else {
			updatedCount++
		}
		results = append(results, map[string]any{
			"id":     saved.ID,
			"email":  saved.Email,
			"status": map[bool]string{true: "created", false: "updated"}[isCreated],
		})
	}
	return map[string]any{
		"ok":       failedCount == 0,
		"total":    len(records),
		"uploaded": createdCount + updatedCount,
		"created":  createdCount,
		"updated":  updatedCount,
		"failed":   failedCount,
		"errors":   errorsList,
		"results":  results,
	}
}

func accountToQuickRecord(a Account) QuickAccountRecord {
	var blob map[string]any
	if len(a.Upstream) > 0 {
		_ = json.Unmarshal(a.Upstream, &blob)
	}
	tokens := parseNestedMap(blob["tokens"])
	client := parseNestedMap(blob["client"])
	discovery := parseNestedMap(blob["discovery"])
	userInfo := parseNestedMap(blob["user_info"])

	tenantURL := pickMapStr(blob, "tenant_url")
	if tenantURL == "" {
		tenantURL = a.Session.TenantURL
	}
	region := pickMapStr(blob, "region")
	if region == "" {
		region = a.Region
	}
	if region == "" {
		region = "us-east-1"
	}
	idcRegion := pickMapStr(client, "idc_region")
	if idcRegion == "" {
		idcRegion = pickMapStr(discovery, "idc_region")
	}
	if idcRegion == "" {
		idcRegion = region
	}
	issuerURL := pickMapStr(client, "issuer_url")
	if issuerURL == "" {
		issuerURL = pickMapStr(discovery, "issuer_url")
	}
	awsAccount := a.AWSAccount
	if awsAccount == "" {
		awsAccount = pickMapStr(userInfo, "aws_account_id")
	}
	if awsAccount == "" {
		awsAccount = pickMapStr(discovery, "account")
	}
	obtainedAt := pickMapInt64(blob, "obtained_at")
	if obtainedAt == 0 {
		obtainedAt = a.Session.ObtainedAt
	}

	return QuickAccountRecord{
		Format:        quickFormatVersion,
		ID:            a.ID,
		Email:         a.Email,
		Label:         a.Label,
		DisplayName:   a.DisplayName,
		Provider:      a.Provider,
		BuildStage:    pickMapStr(blob, "build_stage"),
		Region:        region,
		IDCRegion:     idcRegion,
		IssuerURL:     issuerURL,
		TenantURL:     tenantURL,
		RedirectURI:   pickMapStr(blob, "redirect_uri"),
		AWSAccountID:  awsAccount,
		AccountName:   a.Session.AccountName,
		IDCInstanceID: pickMapStr(discovery, "idc_instance_id"),
		ClientID:      pickMapStr(client, "client_id"),
		ClientSecret:  pickMapStr(client, "client_secret"),
		RefreshToken:  pickMapStr(tokens, "refresh_token"),
		AccessToken:   pickMapStr(tokens, "access_token"),
		IDToken:       pickMapStr(tokens, "id_token"),
		ExpiresIn:     pickMapInt64(tokens, "expires_in"),
		ObtainedAt:    obtainedAt,
		Proxy:         a.Proxy,
		UserInfo:      userInfo,
		Discovery:     discovery,
	}
}
