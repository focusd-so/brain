package brain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/genai"
	"gorm.io/gorm"

	brainv1 "github.com/focusd-so/brain/gen/brain/v1"
	commonv1 "github.com/focusd-so/brain/gen/common/v1"
)

// Cache TTL: 24 hours in seconds
const cacheTTLSeconds = 86400

// Prompts for classification
const promptDesktop = `
You are a Productivity Analyst. Your job is to analyze desktop application entries and classify them based on their impact on focus and productivity.

You will receive:
- **name** (string): The desktop application's name  
- **title** (string, optional): The active window or document title  
- **bundle_id** (string, optional): The app's unique identifier  

You must immediately reply **only with a single, raw JSON object**.  
Do **not** wrap the JSON in markdown fences, do **not** add explanations, and do **not** output anything except the JSON object.

---

## JSON Schema (strict)

The JSON object you return must contain exactly these keys:

1. **"classification"** — one of:
   - "productive"
   - "supporting"
   - "neutral"
   - "distracting"

2. **"reasoning"** — a brief explanation for the classification.

3. **"tags"** — an array containing one or more of the following strictly allowed tags:

[
  "work",
  "research",
  "learning",
  "communication",
  "productivity",
  "content-consumption",
  "social-media",
  "entertainment",
  "news",
  "time-sink",
  "supporting-audio",
  "code-editor",
  "design-tool",
  "other"
]

4. **"detected_project"** — *(string | null)*  
   The inferred project name **only when the application is a code editor**.  
   If no project name can be reliably inferred, return "null".

5. **"confidence_score"** — *(float)*  
   A confidence score between 0.0 and 1.0 indicating the AI's confidence in the classification.

No other keys or tags are permitted.

---

# Classification Rules

Window **context matters**.  
The same app (Slack, Safari, Chrome, Notion, etc.) can fall under different classifications based on its title.

---

## **productive**
Use when the app or its active window directly relates to work or deep focus:

- Coding tools: VS Code, JetBrains IDEs, Terminal, iTerm2  
- Work dashboards: GitHub Desktop, Docker, Cloud consoles  
- Productivity tools: Notion (work pages), Linear, Jira  
- Technical research: docs, API references  
- Learning: tutorials, dev courses

**Slack-specific productive patterns:**
- Channels like:
  - "#incident-*"
  - "#sev*"
  - "#production-alerts"
  - "#engineering", "#backend", "#frontend", "#devops"
- DM or thread windows involving colleagues on work topics
- Any window containing: "PR", "review", "deployment", "on-call"

---

## **supporting**
Use when the app aids focus without being work:

- Music apps: Spotify, Apple Music, Tidal
- Ambient sound apps: Brain.fm, Noisli
- White noise generators
- YouTube / Safari / Chrome **when the title clearly indicates music-only or ambient audio**

Examples:
- "lofi hip hop – beats to relax/study"
- "10 hour rain ambience"
- "deep focus instrumental mix"

Tag with **supporting-audio**.

---

## **neutral**
Use when the app is neither work nor distracting:

- System utilities (Finder, System Settings, Activity Monitor)
- Calculator, Spotlight, basic tools
- File inspectors
- Browser windows with generic or ambiguous searches
- Wikipedia (general knowledge, non-work-specific)

---

## **distracting**
Use when the app or window title indicates entertainment, social media, or attention fragmentation:

- Social media apps: Twitter/X, Instagram, TikTok, Reddit
- Entertainment apps: Netflix, Steam, YouTube homepage or non-music content
- News sites: CNN, NYTimes, Daily Mail
- Games, launchers, streaming platforms
- Browser windows showing addictive or infinite-scroll content

**Slack-specific distracting patterns:**
- Channels like:
  - "#fun-*"
  - "#memes"
  - "#dogs", "#cats"
  - "#random"
  - "#chit-chat"
  - Any channel or window title containing:
  - "fun", "lol", "meme", "offtopic", "social", "pets"

---

# Tagging Rules (simple)

- **work** — coding, documentation, dashboards, reviews
- **research** — technical lookup, factual investigation
- **learning** — tutorials, courses
- **communication** — Slack, Teams, email
- **productivity** — Notion, task managers, calendars
- **content-consumption** — blogs, articles, reading
- **social-media** — X, Reddit, Instagram
- **entertainment** — video, games, streaming
- **news** — general news consumption
- **time-sink** — infinite scroll or addictive feeds
- **supporting-audio** — music or ambient sound aiding focus
- **code-editor** — IDEs and text editors used for coding
- **design-tool** — Figma, Sketch, design software
- **other** — fallback only when no tag applies

---

# Code Editor Project Detection Rules

Populate **"detected_project"** **only when the application is a code editor**
(e.g., VS Code, IntelliJ, GoLand, WebStorm, Neovim, Sublime Text).

Infer the project name from common window title patterns.

### Common patterns to detect:
- "project-name — file.ext"
- "project-name - file.ext"
- "file.ext — project-name"
- "file.ext - project-name"
- "project-name"
- "folder-name (Workspace)"
- "folder-name [SSH]"
- "folder-name — Visual Studio Code"

### Heuristics:
- Prefer **project/folder/workspace name** over file name
- Strip file extensions
- Ignore editor branding ("Visual Studio Code", "IntelliJ IDEA", etc.)
- Ignore temporary labels like "•", "*", "modified"
- If multiple candidates exist, choose the most stable workspace-level name
- If no reliable project name is found, return "null"

---

## **Detected Project Examples**

### Example 1
**Input**
- name: "Visual Studio Code"
- title: "focusd-backend — main.go"
- bundle_id: "com.microsoft.VSCode"

**Output**
{
  "classification": "productive",
  "reasoning": "Actively editing backend source code.",
  "tags": ["work", "code-editor"],
  "detected_project": "focusd-backend",
  "confidence_score": 0.9
}

### Example 2
**Input**
- name: "GoLand"
- title: "auth_service - handler.go"
- bundle_id: "com.jetbrains.goland"

**Output**
{
  "classification": "productive",
  "reasoning": "Backend service development work.",
  "tags": ["work", "code-editor"],
  "detected_project": "auth_service",
  "confidence_score": 0.8
}

### Example 3
**Input**

- name: "Visual Studio Code"
- title: "README"
- bundle_id: "com.microsoft.VSCode"

**Output**
{
  "classification": "productive",
  "reasoning": "Code editor open but project name is not clearly identifiable.",
  "tags": ["work", "code-editor"],
  "detected_project": null,
  "confidence_score": 1
}

### Example 4
**Input**

- name: "Google Antigravity"
- title: "omniquery — Implementation Plan"
- bundle_id: "com.google.antigravity"

**Output**
{
  "classification": "productive",
  "reasoning": "Code editor open but project name is not clearly identifiable.",
  "tags": ["work", "code-editor"],
  "detected_project": "omniquery",
  "confidence_score": 0.7
}

---

# Contextual Interpretation Rules
You must infer intent based on name + title + bundle_id.

Slack Examples
Slack + #incident-1234 → productive (work, communication)

Slack + #fun-dogs → distracting (social-media, entertainment)
Slack + #engineering → productive
Slack + random → distracting unless clearly work-related
Slack + DM with coworker → productive unless clearly casual

### Notion Examples
Notion + roadmap, tasks, planning → productive
Notion + personal journal → neutral
Notion + recipes or travel planning → distracting

Always choose the classification that most accurately reflects how the app affects the user's focus at that moment.

REMINDER: output must be a valid JSON object with no markdown fences, no explanations, and no other text.
`

const promptWebsite = `
You are a Productivity Analyst. Your job is to analyze website entries and classify them based on their impact on focus and productivity.

When given a website URL, title, and optionally metadata (description, OG tags), you must immediately reply **only with a single, raw JSON object**.  
Do **not** wrap the JSON in markdown fences, do **not** add explanations, and do **not** output anything except the JSON object.

---

## JSON Schema (strict)

The JSON object you return must contain exactly these keys:

1. **"classification"** — one of:
   - "productive"
   - "supporting"
   - "neutral"
   - "distracting"
2. **"reasoning"** — a brief explanation for why you chose that classification.
3. **"tags"** — an array containing one or more of the following strictly allowed tags:
[
	"work",
	"research",
	"learning",
	"communication",
	"productivity",
	"content-consumption",
	"social-media",
	"entertainment",
	"news",
	"time-sink",
	"supporting-audio",
	"other"
]

4. **"confidence_score"** — *(float)*  
   A confidence score between 0.0 and 1.0 indicating the AI's confidence in the classification.

---

No other tags are permitted.

---

## Classification Rules

### **focused**
Use this classification when the site directly supports work or skill development:
- coding, PRs, documentation  
- work dashboards or consoles  
- research used for work tasks  
- structured learning or tutorials  
- productivity tools (Notion, Jira, Linear)

Examples: GitHub PR, StackOverflow, MDN, AWS Console, Notion task board.

---

### **supporting**
Use when the site helps maintain focus:
- music players  
- ambient noise  
- lofi playlists  
- audio-only pages intended to reduce distraction  

Examples: Spotify playlist, YouTube Music, Brain.fm.

---

### **neutral**
Use when the site is:
- informational but not work (Wikipedia, dictionary)  
- general-purpose (Google homepage, search results)  
- utility-based (calculators, converters)

Examples: Wikipedia article, Google search result page.

---

### **distracting**
Use for sites that pull attention away from focused work:
- social media feeds  
- entertainment platforms  
- general news  
- algorithmic recommendation feeds  
- meme sites, casual browsing

Examples: Reddit, Instagram, TikTok, YouTube homepage, CNN.

---

## Tagging Rules (simple version)

- **work** — coding, documentation, PRs, dashboards  
- **research** — reading technical or factual content  
- **learning** — tutorials, courses, educational platforms  
- **communication** — Slack, email, messaging  
- **productivity** — tools used for planning, organizing, managing tasks  
- **content-consumption** — articles, blogs, videos unrelated to work  
- **social-media** — X/Twitter, Instagram, Reddit feeds  
- **entertainment** — Netflix, YouTube non-music videos  
- **news** — general news sites  
- **time-sink** — infinite scroll, high-distraction feeds  
- **supporting-audio** — music or ambient sound used for focus  
- **other** — when none of the above meaningfully apply

---

## Examples

### Example 1 — GitHub PR
{
	"classification": "focused",
	"reasoning": "A GitHub PR is directly tied to coding and work output.",
	"tags": ["work", "productivity"],
	"confidence_score": 1
}

### Example 2 — YouTube Music playlist
{
	"classification": "supporting",
	"reasoning": "A music playlist that aids focus without visual distraction.",
	"tags": ["supporting-audio"],
	"confidence_score": 1
}

### Example 3 — Wikipedia article
{
	"classification": "neutral",
	"reasoning": "General informational content not tied to productivity or distraction.",
	"tags": ["research"],
	"confidence_score": 1
}


### Example 4 — Medium article
{
	"classification": "distracting",
	"reasoning": "Medium is a social media platform with high distraction potential.",
	"tags": ["social-media", "time-sink", "entertainment"],
	"confidence_score": 1
}

### Example 5 — News website
{
	"classification": "distracting",
	"reasoning": "News website is a general information site with high distraction potential.",
	"tags": ["news", "time-sink"],
	"confidence_score": 1
}

### Example 6 — Reddit home feed, X/Twitter home feed
{
	"classification": "distracting",
	"reasoning": "Reddit is a social platform with high distraction potential.",
	"tags": ["social-media", "time-sink", "entertainment"],
	"confidence_score": 1
}

---

Use metadata, page title, and URL patterns to improve accuracy.
`

// ClassificationResult represents the AI response structure for applications
type ClassificationResult struct {
	Classification  string   `json:"classification"`
	Reasoning       string   `json:"reasoning"`
	Tags            []string `json:"tags"`
	DetectedProject *string  `json:"detected_project"`
	ConfidenceScore float32  `json:"confidence_score"`
}

// WebsiteClassificationResult represents the AI response structure for websites
type WebsiteClassificationResult struct {
	Classification  string   `json:"classification"`
	Reasoning       string   `json:"reasoning"`
	Tags            []string `json:"tags"`
	ConfidenceScore float64  `json:"confidence_score"`
}

// ClassificationService handles AI-powered classification
type ClassificationService struct {
	db     *gorm.DB
	client *genai.Client
}

// NewClassificationService creates a new classification service
func NewClassificationService(db *gorm.DB) (*ClassificationService, error) {
	ctx := context.Background()

	// Try GOOGLE_API_KEY first, then GEMINI_API_KEY
	apiKey := os.Getenv("GOOGLE_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("FOCUSD_GEMINI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("Gemini API key not found (tried GOOGLE_API_KEY, GEMINI_API_KEY, FOCUSD_GEMINI_API_KEY)")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini client: %w", err)
	}

	return &ClassificationService{
		db:     db,
		client: client,
	}, nil
}

// ClassifyApplication classifies a desktop application
func (s *ServiceImpl) ClassifyApplication(ctx context.Context, req *connect.Request[brainv1.ClassifyApplicationRequest]) (*connect.Response[brainv1.ClassifyApplicationResponse], error) {
	cs, err := NewClassificationService(s.gormDB)
	if err != nil {
		slog.Error("failed to create classification service", "error", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("classification service error: %w", err))
	}

	contextData := map[string]string{
		"name":      req.Msg.ApplicationName,
		"title":     req.Msg.WindowTitle,
		"bundle_id": req.Msg.ApplicationBundleId,
	}

	result, err := cs.classifyWithCache(ctx, promptDesktop, contextData)
	if err != nil {
		slog.Error("classification failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("classification failed: %w", err))
	}

	var classification ClassificationResult
	if err := json.Unmarshal([]byte(result), &classification); err != nil {
		slog.Error("failed to parse classification result", "error", err, "result", result)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to parse classification: %w", err))
	}

	response := &brainv1.ClassifyApplicationResponse{
		Classification: &brainv1.ClassificationResult{
			Classification:  classification.Classification,
			Reasoning:       classification.Reasoning,
			Tags:            classification.Tags,
			ConfidenceScore: classification.ConfidenceScore,
		},
	}

	if classification.DetectedProject != nil && *classification.DetectedProject != "null" {
		response.DetectedProject = classification.DetectedProject
	}

	return connect.NewResponse(response), nil
}

// ClassifyWebsite classifies a website URL
func (s *ServiceImpl) ClassifyWebsite(ctx context.Context, req *connect.Request[brainv1.ClassifyWebsiteRequest]) (*connect.Response[brainv1.ClassifyWebsiteResponse], error) {
	cs, err := NewClassificationService(s.gormDB)
	if err != nil {
		slog.Error("failed to create classification service", "error", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("classification service error: %w", err))
	}

	// Fetch website metadata with timeout
	metadata := fetchWebsiteMetadata(req.Msg.Url)

	contextData := map[string]string{
		"url": req.Msg.Url,
	}

	// Add title from request or fetched metadata
	if req.Msg.Title != "" {
		contextData["title"] = req.Msg.Title
	} else if metadata.Title != "" {
		contextData["title"] = metadata.Title
	}

	if metadata.Description != "" {
		contextData["description"] = metadata.Description
	}
	if metadata.Keywords != "" {
		contextData["keywords"] = metadata.Keywords
	}

	result, err := cs.classifyWithCache(ctx, promptWebsite, contextData)
	if err != nil {
		slog.Error("classification failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("classification failed: %w", err))
	}

	var classification WebsiteClassificationResult
	if err := json.Unmarshal([]byte(result), &classification); err != nil {
		slog.Error("failed to parse classification result", "error", err, "result", result)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to parse classification: %w", err))
	}

	return connect.NewResponse(&brainv1.ClassifyWebsiteResponse{
		Classification: &brainv1.ClassificationResult{
			Classification: classification.Classification,
			Reasoning:      classification.Reasoning,
			Tags:           classification.Tags,
		},
	}), nil
}

// classifyWithCache performs classification with caching
func (cs *ClassificationService) classifyWithCache(ctx context.Context, prompt string, contextData map[string]string) (string, error) {
	// Generate cache key
	cacheKey := generateCacheKey(prompt, contextData)

	// Check cache
	cached, err := cs.getFromCache(cacheKey)
	if err == nil && cached != "" {
		slog.Debug("cache hit", "key", cacheKey[:16])
		return cached, nil
	}

	slog.Debug("cache miss", "key", cacheKey[:16])

	// Call Gemini
	result, err := cs.callGemini(ctx, prompt, contextData)
	if err != nil {
		return "", err
	}

	// Store in cache (non-blocking)
	go func() {
		if storeErr := cs.storeInCache(cacheKey, result); storeErr != nil {
			slog.Error("failed to store in cache", "error", storeErr)
		}
	}()

	return result, nil
}

// callGemini calls the Gemini API for classification
func (cs *ClassificationService) callGemini(ctx context.Context, prompt string, contextData map[string]string) (string, error) {
	contextJSON, err := json.Marshal(contextData)
	if err != nil {
		return "", fmt.Errorf("failed to marshal context data: %w", err)
	}

	resp, err := cs.client.Models.GenerateContent(ctx, "gemini-1.5-flash", []*genai.Content{
		{
			Role: "user",
			Parts: []*genai.Part{
				genai.NewPartFromText(string(contextJSON)),
			},
		},
	}, &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{
				genai.NewPartFromText(prompt),
			},
		},
		ResponseMIMEType: "application/json",
	})
	if err != nil {
		return "", fmt.Errorf("gemini API error: %w", err)
	}

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty response from Gemini")
	}

	text := resp.Candidates[0].Content.Parts[0].Text

	// Clean up response (remove markdown fences if present)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	return text, nil
}

// generateCacheKey creates a SHA-256 hash of prompt + context
func generateCacheKey(prompt string, contextData map[string]string) string {
	// Sort keys for deterministic serialization
	sortedJSON, _ := json.Marshal(contextData)
	input := prompt + ":" + string(sortedJSON)

	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])
}

// getFromCache retrieves a cached response
func (cs *ClassificationService) getFromCache(hash string) (string, error) {
	var cache commonv1.PromptHistoryORM
	err := cs.db.Where("prompt_hash = ? AND expires_at > ?", hash, time.Now().Unix()).First(&cache).Error
	if err != nil {
		return "", err
	}
	return cache.ResponseJson, nil
}

// storeInCache stores a response in the cache
func (cs *ClassificationService) storeInCache(hash, response string) error {
	now := time.Now().Unix()
	cache := commonv1.PromptHistoryORM{
		PromptHash:   hash,
		ResponseJson: response,
		CreatedAt:    now,
		ExpiresAt:    now + cacheTTLSeconds,
	}

	// Use upsert to handle race conditions
	return cs.db.Save(&cache).Error
}

// WebsiteMetadata holds fetched metadata from a URL
type WebsiteMetadata struct {
	Title       string
	Description string
	Keywords    string
}

// fetchWebsiteMetadata fetches metadata from a URL with a 200ms timeout
func fetchWebsiteMetadata(url string) WebsiteMetadata {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return WebsiteMetadata{}
	}

	req.Header.Set("User-Agent", "FocusdBot/1.0")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return WebsiteMetadata{}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return WebsiteMetadata{}
	}

	// Read limited body to avoid memory issues
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024)) // 64KB limit
	if err != nil {
		return WebsiteMetadata{}
	}

	html := string(body)
	return extractMetadata(html)
}

// extractMetadata extracts title, description, and keywords from HTML
func extractMetadata(html string) WebsiteMetadata {
	var metadata WebsiteMetadata

	// Extract title
	titleRegex := regexp.MustCompile(`(?i)<title>(.*?)</title>`)
	ogTitleRegex := regexp.MustCompile(`(?i)<meta\s+property=["']og:title["']\s+content=["'](.*?)["']`)

	if matches := ogTitleRegex.FindStringSubmatch(html); len(matches) > 1 {
		metadata.Title = matches[1]
	} else if matches := titleRegex.FindStringSubmatch(html); len(matches) > 1 {
		metadata.Title = matches[1]
	}

	// Extract description
	descRegex := regexp.MustCompile(`(?i)<meta\s+name=["']description["']\s+content=["'](.*?)["']`)
	ogDescRegex := regexp.MustCompile(`(?i)<meta\s+property=["']og:description["']\s+content=["'](.*?)["']`)

	if matches := ogDescRegex.FindStringSubmatch(html); len(matches) > 1 {
		metadata.Description = matches[1]
	} else if matches := descRegex.FindStringSubmatch(html); len(matches) > 1 {
		metadata.Description = matches[1]
	}

	// Extract keywords
	keywordsRegex := regexp.MustCompile(`(?i)<meta\s+name=["']keywords["']\s+content=["'](.*?)["']`)
	if matches := keywordsRegex.FindStringSubmatch(html); len(matches) > 1 {
		metadata.Keywords = matches[1]
	}

	return metadata
}
