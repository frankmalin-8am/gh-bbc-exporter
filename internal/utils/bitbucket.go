package utils

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/katiem0/gh-bbc-exporter/internal/data"
	"go.uber.org/zap"
)

type Client struct {
	baseURL          string
	httpClient       *http.Client
	accessToken      string // Workspace Access Token
	apiToken         string // API Token replacing AppPass after Sept 2025
	email            string // Will replace username after Sept 2025
	username         string // Will be removed after Sept 2025 with appPass
	appPass          string // To be deprecated Sept 2025
	logger           *zap.Logger
	commitSHACache   map[string]string
	// prParticipantsCache stores the Bitbucket participants embedded in each
	// PR response, keyed by the GitHub-format PR URL (e.g.
	// "https://bitbucket.org/ws/repo/pull/22").  Populated during GetPullRequests
	// and consumed by GetPullRequestApprovals so no extra API call is needed.
	prParticipantsCache map[string][]data.BitbucketParticipant
	exportDir           string
	skipCommitLookup    bool
	shaFallback         string // "none" | "related" | "nearest"
	// activeUserUUIDs is the set of UUIDs (braces stripped) that are current
	// workspace members.  Populated by GetUsers so that GetPullRequests and
	// GetPullRequestComments can choose the right URL format per user.
	activeUserUUIDs map[string]bool
	// inactiveUsers collects users referenced in PRs/comments whose UUID is not
	// in activeUserUUIDs.  They are emitted with a nickname-based URL so that
	// the resulting mannequin in GitHub has a human-readable name.  Keyed by
	// clean UUID (no braces).
	inactiveUsers map[string]data.User
}

func NewClient(baseURL, accessToken, apiToken, email, username, appPass string, logger *zap.Logger, exportDir string, skipCommitLookup bool, shaFallback string) *Client {
	baseURL = strings.TrimSuffix(baseURL, "/")

	if !strings.Contains(baseURL, "/2.0") && strings.Contains(baseURL, "api.bitbucket.org") {
		baseURL = baseURL + "/2.0"
	}

	var authMethod string
	if accessToken != "" {
		authMethod = "workspace access token"
	} else if apiToken != "" {
		if email != "" {
			authMethod = "API token with email"
		} else {
			authMethod = "API token with x-bitbucket-api-token-auth"
		}
	} else if username != "" && appPass != "" {
		authMethod = "username and app password"
	} else {
		authMethod = "none"
	}

	logger.Debug("Creating Bitbucket client",
		zap.String("baseURL", baseURL),
		zap.String("authMethod", authMethod))

	return &Client{
		baseURL:             baseURL,
		httpClient:          &http.Client{},
		accessToken:         accessToken,
		apiToken:            apiToken,
		email:               email,
		username:            username,
		appPass:             appPass,
		logger:              logger,
		commitSHACache:      make(map[string]string),
		prParticipantsCache: make(map[string][]data.BitbucketParticipant),
		exportDir:           exportDir,
		skipCommitLookup:    skipCommitLookup,
		shaFallback:         shaFallback,
		activeUserUUIDs:     make(map[string]bool),
		inactiveUsers:       make(map[string]data.User),
	}
}

func (c *Client) GetRepository(workspace, repoSlug string) (*data.BitbucketRepository, error) {
	endpoint := fmt.Sprintf("/repositories/%s/%s", workspace, repoSlug)

	c.logger.Debug("Fetching repository",
		zap.String("workspace", workspace),
		zap.String("repository", repoSlug))

	var repo data.BitbucketRepository
	err := c.makeRequest("GET", endpoint, &repo)
	if err != nil {
		c.logger.Error("Failed to fetch repository details",
			zap.Error(err),
			zap.String("endpoint", endpoint))
		return nil, err
	}
	if repo.MainBranch != nil {
		c.logger.Debug("Repository main branch",
			zap.String("branch", repo.MainBranch.Name))
	} else {
		c.logger.Debug("Repository has no main branch defined")
	}

	return &repo, nil
}

func (c *Client) makeRequest(method, endpoint string, v interface{}) error {
	var fullURL string
	maxRetries := 5
	baseDelay := 1 * time.Second

	for attempt := 0; attempt < maxRetries; attempt++ {
		if strings.HasPrefix(endpoint, c.baseURL) {
			fullURL = endpoint
		} else {
			endpointPath := endpoint
			queryParams := ""

			if strings.Contains(endpoint, "?") {
				parts := strings.SplitN(endpoint, "?", 2)
				endpointPath = parts[0]
				queryParams = parts[1]
			}

			baseURL := strings.TrimSuffix(c.baseURL, "/")
			endpointPath = strings.TrimPrefix(endpointPath, "/")

			// Build the full URL
			if queryParams != "" {
				fullURL = fmt.Sprintf("%s/%s?%s", baseURL, endpointPath, queryParams)
			} else {
				fullURL = fmt.Sprintf("%s/%s", baseURL, endpointPath)
			}
		}

		if attempt == 0 {
			c.logger.Debug("Making API request",
				zap.String("method", method),
				zap.String("url", fullURL))
		}

		req, err := http.NewRequest(method, fullURL, nil)
		if err != nil {
			return err
		}

		if c.accessToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.accessToken)
		} else if c.apiToken != "" {
			if c.email != "" {
				req.SetBasicAuth(c.email, c.apiToken)
			} else {
				req.SetBasicAuth("x-bitbucket-api-token-auth", c.apiToken)
			}
		} else if c.username != "" && c.appPass != "" {
			req.SetBasicAuth(c.username, c.appPass)
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			err := resp.Body.Close()
			if err != nil {
				c.logger.Warn("Error closing response body", zap.Error(err))
			}
		}()

		remaining := resp.Header.Get("X-RateLimit-Remaining")
		limit := resp.Header.Get("X-RateLimit-Limit")
		if remaining != "" && limit != "" {
			remainingInt, _ := strconv.Atoi(remaining)
			limitInt, _ := strconv.Atoi(limit)
			if limitInt > 0 && float64(remainingInt)/float64(limitInt) < 0.1 {
				c.logger.Warn("Low API rate limit remaining",
					zap.String("remaining", remaining),
					zap.String("limit", limit))
			}
		}

		// Only log non-successful responses
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.logger.Debug("API response",
				zap.Int("status", resp.StatusCode),
				zap.String("status_text", resp.Status))
		}
		if resp.StatusCode == 429 {
			delay := baseDelay * time.Duration(1<<attempt) // Exponential backoff
			if delay > 5*time.Minute {
				delay = 5 * time.Minute // Max delay
			}

			c.logger.Warn("Rate limit hit - waiting before retrying",
				zap.Duration("delay", delay),
				zap.Int("attempt", attempt+1),
				zap.Int("max_retries", maxRetries))

			time.Sleep(delay)
			continue // Retry the request
		}

		// If the request was successful, break out of the retry loop
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return json.NewDecoder(resp.Body).Decode(v)
		}

		// Handle other errors
		bodyBytes, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("API request failed with status %d: %s: %s",
			resp.StatusCode, resp.Status, string(bodyBytes))
		c.logger.Error("API request failed", zap.Error(err))
		return err
	}

	return fmt.Errorf("API request failed after %d retries", maxRetries)
}

func (c *Client) GetUsers(workspace, repoSlug string) ([]data.User, error) {
	c.logger.Info("Fetching workspace members")

	var allUsers []data.User
	page := 1
	pageLen := 100
	hasMore := true

	for hasMore {
		endpoint := fmt.Sprintf("workspaces/%s/members?page=%d&pagelen=%d",
			workspace, page, pageLen)

		var response data.BitbucketUserResponse

		err := c.makeRequest("GET", endpoint, &response)
		if err != nil {
			c.logger.Warn("Failed to fetch workspace members, using fallback user",
				zap.Error(err))
			return []data.User{
				{
					Type:      "user",
					URL:       fmt.Sprintf("https://bitbucket.org/%s", workspace),
					Login:     workspace,
					Name:      workspace,
					Company:   nil,
					Website:   nil,
					Location:  nil,
					Emails:    nil,
					CreatedAt: formatDateToZ(time.Now().Format(time.RFC3339)),
				},
			}, nil
		}

		for _, member := range response.Values {
			user := member.User

			profileURL := fmt.Sprintf("https://bitbucket.org/%s", strings.Trim(user.UUID, "{}"))

			newUser := data.User{
				Type:      "user",
				URL:       profileURL,
				Login:     strings.Trim(user.UUID, "{}"),
				Name:      user.DisplayName,
				Company:   nil,
				Website:   nil,
				Location:  nil,
				Emails:    nil,
				CreatedAt: formatDateToZ(time.Now().Format(time.RFC3339)),
			}

			allUsers = append(allUsers, newUser)
		}

		hasMore = response.Next != ""
		if hasMore {
			page++
		}
	}
	if len(allUsers) == 0 {
		c.logger.Warn("No workspace members found, using fallback user")
		allUsers = append(allUsers, data.User{
			Type:      "user",
			URL:       fmt.Sprintf("https://bitbucket.org/%s", workspace),
			Login:     workspace,
			Name:      workspace,
			Company:   nil,
			Website:   nil,
			Location:  nil,
			Emails:    nil,
			CreatedAt: formatDateToZ(time.Now().Format(time.RFC3339)),
		})
	}

	// Always ensure the workspace system user is present so that migration
	// summary comments (attributed to this URL) resolve to a valid mannequin.
	wsURL := fmt.Sprintf("https://bitbucket.org/%s", workspace)
	found := false
	for _, u := range allUsers {
		if u.URL == wsURL {
			found = true
			break
		}
	}
	if !found {
		allUsers = append(allUsers, data.User{
			Type:      "user",
			URL:       wsURL,
			Login:     workspace,
			Name:      workspace,
			Company:   nil,
			Website:   nil,
			Location:  nil,
			Emails:    []data.Email{},
			CreatedAt: formatDateToZ(time.Now().Format(time.RFC3339)),
		})
		c.logger.Debug("Added workspace system user for migration attribution",
			zap.String("url", wsURL))
	}

	// Cache active UUIDs so GetPullRequests / GetPullRequestComments can
	// distinguish current members from former members.
	if c.activeUserUUIDs == nil {
		c.activeUserUUIDs = make(map[string]bool)
	}
	for _, u := range allUsers {
		if u.Login != "" && u.Login != workspace {
			c.activeUserUUIDs[u.Login] = true
		}
	}

	c.logger.Debug("Fetched workspace members",
		zap.Int("count", len(allUsers)))

	return allUsers, nil
}

// resolveUserURL returns the canonical URL for a Bitbucket user in the GEI
// archive format.  For users who are still active workspace members the URL
// is UUID-based (https://bitbucket.org/<uuid>), which lets the mannequin be
// reclaimed by the real GitHub user later.  For users who have left the
// workspace the URL is nickname-based (https://bitbucket.org/<nickname>),
// which produces a human-readable mannequin name instead of an opaque UUID.
// Inactive users are registered in c.inactiveUsers so the exporter can append
// them to users_000001.json.
func (c *Client) resolveUserURL(workspace, uuid, nickname, displayName string) string {
	// Lazy-initialize maps so tests that build Client literals directly still work.
	if c.activeUserUUIDs == nil {
		c.activeUserUUIDs = make(map[string]bool)
	}
	if c.inactiveUsers == nil {
		c.inactiveUsers = make(map[string]data.User)
	}

	cleanUUID := strings.Trim(uuid, "{}")

	if c.activeUserUUIDs[cleanUUID] {
		// Active member — keep UUID-based URL for mannequin reclaim support.
		return fmt.Sprintf("https://bitbucket.org/%s", cleanUUID)
	}

	// Inactive / unknown user — fall back to nickname for readability.
	login := nickname
	if login == "" {
		login = cleanUUID // last resort: still unique, just not human-readable
	}
	userURL := fmt.Sprintf("https://bitbucket.org/%s", login)

	// Register so the exporter can add this user to users_000001.json.
	if _, known := c.inactiveUsers[cleanUUID]; !known {
		name := displayName
		if name == "" {
			name = login
		}
		c.inactiveUsers[cleanUUID] = data.User{
			Type:      "user",
			URL:       userURL,
			Login:     login,
			Name:      name,
			Emails:    []data.Email{},
			CreatedAt: formatDateToZ(time.Now().Format(time.RFC3339)),
		}
		c.logger.Debug("Discovered inactive user — using nickname-based URL",
			zap.String("uuid", cleanUUID),
			zap.String("nickname", login),
			zap.String("url", userURL))
	}

	return userURL
}

// GetInactiveUsers returns all users discovered during PR/comment processing
// whose UUID was not found in the active workspace member list.  The exporter
// should merge these into users_000001.json after all PR data is fetched.
func (c *Client) GetInactiveUsers() []data.User {
	users := make([]data.User, 0, len(c.inactiveUsers))
	for _, u := range c.inactiveUsers {
		users = append(users, u)
	}
	return users
}

func (c *Client) GetPullRequests(workspace, repoSlug string, openPRsOnly bool, prsFromDate string) ([]data.PullRequest, error) {
	c.logger.Info("Fetching pull requests",
		zap.String("workspace", workspace),
		zap.String("repository", repoSlug),
		zap.Bool("open_prs_only", openPRsOnly))

	var pullRequests []data.PullRequest
	page := 1
	pageLen := 50
	hasMore := true

	var fromDate time.Time
	var fromDateProvided bool

	if prsFromDate != "" {
		parsedDate, err := time.Parse("2006-01-02", prsFromDate)
		if err != nil {
			c.logger.Error("Failed to parse from date",
				zap.String("date", prsFromDate),
				zap.Error(err))
			return nil, fmt.Errorf("invalid date format for prsFromDate: %w", err)
		}

		// Set time to beginning of day in UTC to ensure consistent comparisons
		fromDate = time.Date(
			parsedDate.Year(),
			parsedDate.Month(),
			parsedDate.Day(),
			0, 0, 0, 0,
			time.UTC)

		fromDateProvided = true
		c.logger.Info("Filtering PRs by creation date",
			zap.String("from_date", prsFromDate))
	}

	skippedAmbiguous := 0
	skippedByDate := 0

	for hasMore {
		baseURL, parseErr := url.Parse(fmt.Sprintf("repositories/%s/%s/pullrequests", workspace, repoSlug))
		if parseErr != nil {
			c.logger.Error("failed to parse base URL", zap.Error(parseErr))
			return nil, parseErr
		}

		queryParams := url.Values{}
		queryParams.Set("page", strconv.Itoa(page))
		queryParams.Set("pagelen", strconv.Itoa(pageLen))

		if openPRsOnly {
			queryParams.Set("state", "OPEN")
		} else {
			queryParams.Set("state", "ALL")
		}

		baseURL.RawQuery = queryParams.Encode()
		endpoint := baseURL.String()

		var response data.BitbucketPRResponse
		var err error

		maxRetries := 3
		for retries := 0; retries < maxRetries; retries++ {
			err = c.makeRequest("GET", endpoint, &response)
			if err != nil {
				c.logger.Warn("API request error",
					zap.String("endpoint", endpoint),
					zap.Int("retry", retries),
					zap.Error(err))
				time.Sleep(time.Duration(500*(retries+1)) * time.Millisecond)
				continue
			}
			break
		}
		if err != nil {
			c.logger.Error("failed to fetch pull requests", zap.Error(err))
			return nil, err
		}

		for _, pr := range response.Values {

			if hexPatternRegex.MatchString(pr.Source.Branch.Name) {
				skippedAmbiguous++
				c.logger.Debug("Skipping PR with ambiguous source branch",
					zap.Int("pr_id", pr.ID),
					zap.String("branch_name", pr.Source.Branch.Name))
				continue
			}

			if hexPatternRegex.MatchString(pr.Destination.Branch.Name) {
				skippedAmbiguous++
				c.logger.Debug("Skipping PR with ambiguous destination branch",
					zap.Int("pr_id", pr.ID),
					zap.String("branch_name", pr.Destination.Branch.Name))
				continue
			}

			if fromDateProvided {
				prCreatedAt, err := time.Parse(time.RFC3339, pr.CreatedOn)
				if err != nil {
					c.logger.Warn("Could not parse PR creation date",
						zap.String("date", pr.CreatedOn),
						zap.Int("pr_id", pr.ID),
						zap.Error(err))
					continue
				}

				if prCreatedAt.Before(fromDate) {
					skippedByDate++
					continue
				}

				c.logger.Debug("Including PR: creation date meets filter criteria",
					zap.Int("pr_id", pr.ID),
					zap.String("pr_title", pr.Title),
					zap.Time("pr_created_at", prCreatedAt))
			}

			var mergedAt, closedAt *string
			switch pr.State {
			case "MERGED":
				mergedStr := formatDateToZ(pr.UpdatedOn)
				mergedAt = &mergedStr
				closedStr := formatDateToZ(pr.UpdatedOn)
				closedAt = &closedStr
			case "DECLINED":
				closedStr := formatDateToZ(pr.UpdatedOn)
				closedAt = &closedStr
			}

			prURL := formatURL("pr", workspace, repoSlug, pr.ID)
			userURL := c.resolveUserURL(workspace, pr.Author.UUID, pr.Author.Nickname, pr.Author.DisplayName)
			repoURL := formatURL("repository", workspace, repoSlug)
			prUser := formatURL("user", workspace, "")

			// Resolve commit SHAs
			baseSHA, _ := c.GetFullCommitSHA(workspace, repoSlug, pr.Destination.Commit.Hash)
			headSHA, _ := c.GetFullCommitSHA(workspace, repoSlug, pr.Source.Commit.Hash)

			// Format merge commit SHA if available.  This must be resolved before
			// the headSHA fallback below so it can be used as the fallback value.
			// If the merge commit is unresolvable (short SHA), leave the pointer nil
			// rather than storing a dangling reference — GEI treats nil the same as
			// an absent field, which is cleaner than an invalid short SHA.
			var mergeCommitSHA *string
			if pr.MergeCommit != nil && pr.State == "MERGED" {
				fullMergeSHA, _ := c.GetFullCommitSHA(workspace, repoSlug, pr.MergeCommit.Hash)
				if len(fullMergeSHA) == 40 {
					mergeCommitSHA = &fullMergeSHA
				} else {
					c.logger.Debug("merge commit SHA unresolvable — omitting from export",
						zap.Int("pr_id", pr.ID),
						zap.String("short_sha", fullMergeSHA))
				}
			}

			// Source-branch and base-branch commits are sometimes inaccessible on
			// old PRs (branch deleted + objects GC'd).  A SHA shorter than 40
			// chars means GetFullCommitSHA couldn't resolve it.  GEI silently
			// drops any PR whose head or base SHA is not a valid 40-char value.
			//
			// Behaviour is controlled by --sha-fallback:
			//   "none"    — no substitution; pass short SHAs through as-is
			//   "related" — headSHA: try merge commit SHA (MERGED), then baseSHA
			//               baseSHA: try headSHA (once resolved), then nearest
			//   "nearest" — all of "related", then fall back to the nearest/oldest
			//               commit on the destination branch from the local clone

			// ── headSHA fallback ────────────────────────────────────────────────
			if c.shaFallback != "none" && len(headSHA) < 40 {
				original := headSHA
				if mergeCommitSHA != nil && len(*mergeCommitSHA) == 40 {
					headSHA = *mergeCommitSHA
					c.logger.Debug("headSHA unresolvable — using merge commit SHA as fallback",
						zap.Int("pr_id", pr.ID),
						zap.String("original_sha", original),
						zap.String("fallback_sha", headSHA))
				} else if len(baseSHA) == 40 {
					headSHA = baseSHA
					c.logger.Debug("headSHA unresolvable — using base SHA as fallback",
						zap.Int("pr_id", pr.ID),
						zap.String("original_sha", original),
						zap.String("fallback_sha", headSHA))
				} else if c.shaFallback == "nearest" {
					nearestSHA := c.getNearestCommitSHA(workspace, repoSlug,
						pr.Destination.Branch.Name, pr.CreatedOn)
					if nearestSHA != "" {
						headSHA = nearestSHA
						c.logger.Debug("headSHA unresolvable — using nearest local commit as fallback",
							zap.Int("pr_id", pr.ID),
							zap.String("original_sha", original),
							zap.String("destination_branch", pr.Destination.Branch.Name),
							zap.String("fallback_sha", headSHA))
					} else {
						c.logger.Warn("PR has no resolvable head SHA — GEI may skip it",
							zap.Int("pr_id", pr.ID),
							zap.String("original_sha", original),
							zap.String("sha_fallback", c.shaFallback))
					}
				} else {
					c.logger.Warn("PR has no resolvable head SHA — GEI may skip it",
						zap.Int("pr_id", pr.ID),
						zap.String("original_sha", original),
						zap.String("sha_fallback", c.shaFallback))
				}
			}

			// ── baseSHA fallback ────────────────────────────────────────────────
			// baseSHA must also be a full 40-char value.  Old destination-branch
			// history can be GC'd just as source-branch history can be.
			if c.shaFallback != "none" && len(baseSHA) < 40 {
				original := baseSHA
				if len(headSHA) == 40 {
					// headSHA is already anchored (original or via fallback above);
					// using it for base is imprecise but keeps the PR importable.
					baseSHA = headSHA
					c.logger.Debug("baseSHA unresolvable — using resolved head SHA as fallback",
						zap.Int("pr_id", pr.ID),
						zap.String("original_sha", original),
						zap.String("fallback_sha", baseSHA))
				} else if c.shaFallback == "nearest" {
					nearestSHA := c.getNearestCommitSHA(workspace, repoSlug,
						pr.Destination.Branch.Name, pr.CreatedOn)
					if nearestSHA != "" {
						baseSHA = nearestSHA
						c.logger.Debug("baseSHA unresolvable — using nearest local commit as fallback",
							zap.Int("pr_id", pr.ID),
							zap.String("original_sha", original),
							zap.String("destination_branch", pr.Destination.Branch.Name),
							zap.String("fallback_sha", baseSHA))
					} else {
						c.logger.Warn("PR has no resolvable base SHA — GEI may skip it",
							zap.Int("pr_id", pr.ID),
							zap.String("original_sha", original),
							zap.String("sha_fallback", c.shaFallback))
					}
				} else {
					c.logger.Warn("PR has no resolvable base SHA — GEI may skip it",
						zap.Int("pr_id", pr.ID),
						zap.String("original_sha", original),
						zap.String("sha_fallback", c.shaFallback))
				}
			}

			description := "📜 _Migrated from Bitbucket: this pull request was opened without a description._"
			if pr.Description != nil && strings.TrimSpace(*pr.Description) != "" {
				description = *pr.Description
			}

			// Create the Pull Request with GitHub-compatible structure
			pullRequest := data.PullRequest{
				Type:       "pull_request",
				URL:        prURL,
				User:       userURL,
				Repository: repoURL,
				Title:      pr.Title,
				Body:       description,
				Base: data.PRBranch{
					Ref:  pr.Destination.Branch.Name,
					SHA:  baseSHA,
					User: prUser,
					Repo: repoURL,
				},
				Head: data.PRBranch{
					Ref:  pr.Source.Branch.Name,
					SHA:  headSHA,
					User: prUser,
					Repo: repoURL,
				},
				Labels:               []string{},
				MergedAt:             mergedAt,
				ClosedAt:             closedAt,
				CreatedAt:            formatDateToZ(pr.CreatedOn),
				Assignee:             nil,
				Assignees:            []string{},
				Milestone:            nil,
				Reactions:            []string{},
				ReviewRequests:       []string{},
				CloseIssueReferences: []string{},
				WorkInProgress:       pr.Draft,
				MergeCommitSHA:       mergeCommitSHA,
			}

			pullRequests = append(pullRequests, pullRequest)

			// Cache the embedded participants so GetPullRequestApprovals can
			// read them without needing a separate /participants API call.
			// Guard against nil map for tests that use &Client{} directly.
			if len(pr.Participants) > 0 && c.prParticipantsCache != nil {
				c.prParticipantsCache[prURL] = pr.Participants
			}
		}

		hasMore = response.Next != ""
		if hasMore {
			page++
		}
	}

	c.logger.Info("Pull requests fetched",
		zap.Int("total", len(pullRequests)),
		zap.Int("skipped_ambiguous", skippedAmbiguous),
		zap.Int("skipped_by_date", skippedByDate))

	return pullRequests, nil
}

func (c *Client) GetFullCommitSHA(workspace, repoSlug, commitHash string) (string, error) {
	if len(commitHash) == 40 {
		return commitHash, nil
	}

	if fullSHA, exists := c.commitSHACache[commitHash]; exists {
		return fullSHA, nil
	}

	repoPath := filepath.Join(c.exportDir, "repositories", workspace, repoSlug+".git")
	if _, err := os.Stat(repoPath); err == nil {
		// Repository exists locally
		fullSHA, err := GetFullCommitSHAFromLocalRepo(repoPath, commitHash)
		if err == nil {
			// Cache the result
			c.commitSHACache[commitHash] = fullSHA
			c.logger.Debug("Resolved full SHA from local repository",
				zap.String("shortSHA", commitHash),
				zap.String("fullSHA", fullSHA))
			return fullSHA, nil
		}
		// If local lookup fails, log and fall back to API
		c.logger.Debug("Failed to get full SHA from local repo, falling back to API",
			zap.String("shortSHA", commitHash),
			zap.Error(err))
	}

	if c.skipCommitLookup {
		c.logger.Warn("Cannot resolve full commit SHA - API lookup disabled,",
			zap.String("workspace", workspace),
			zap.String("repo", repoSlug),
			zap.String("sha", commitHash),
			zap.Int("sha_length", len(commitHash)),
			zap.String("impact", "This may cause failures if full SHA required"))
		c.commitSHACache[commitHash] = commitHash
		return commitHash, nil
	}

	endpoint := fmt.Sprintf("repositories/%s/%s/commit/%s", workspace, repoSlug, commitHash)

	var response struct {
		Hash string `json:"hash"`
	}

	err := c.makeRequest("GET", endpoint, &response)
	if err != nil {
		return commitHash, fmt.Errorf("failed to fetch full commit SHA: %w", err)
	}

	if len(response.Hash) == 40 {
		c.commitSHACache[commitHash] = response.Hash
		return response.Hash, nil
	}

	return commitHash, nil
}

func (c *Client) GetPullRequestComments(workspace, repoSlug string, pullRequests []data.PullRequest) ([]data.IssueComment, []data.PullRequestReviewComment, error) {
	c.logger.Info("Fetching pull request comments")

	var regularComments []data.IssueComment
	var reviewComments []data.PullRequestReviewComment

	prURLMap := make(map[int]string)
	prCommitMap := make(map[int]string)
	prAuthorMap := make(map[int]string) // prID → author UUID (stripped of braces)

	for _, pr := range pullRequests {
		parts := strings.Split(pr.URL, "/")
		if len(parts) > 0 {
			prID, err := strconv.Atoi(parts[len(parts)-1])
			if err == nil {
				prURLMap[prID] = pr.URL
				prCommitMap[prID] = pr.Head.SHA
				// pr.User is the GEI-format URL: "https://bitbucket.org/{uuid}"
			// Extract the UUID from the last path segment.
			userParts := strings.Split(pr.User, "/")
			prAuthorMap[prID] = userParts[len(userParts)-1]
			}
		}
	}

	resolvedSHAs := make(map[int]bool)
	failedPRs := 0

	for prID := range prURLMap {
		page := 1
		pageLen := 100
		hasMore := true

		for hasMore {
			baseEndpoint := fmt.Sprintf("repositories/%s/%s/pullrequests/%d/comments",
				workspace, repoSlug, prID)

			params := url.Values{}
			params.Add("q", "deleted=false")
			params.Add("page", strconv.Itoa(page))
			params.Add("pagelen", strconv.Itoa(pageLen))

			endpoint := baseEndpoint + "?" + params.Encode()

			var response data.BitbucketCommentResponse

			err := c.makeRequest("GET", endpoint, &response)
			if err != nil {
				c.logger.Warn("Failed to fetch PR comments",
					zap.Int("pr_id", prID),
					zap.Error(err))
				break
			}

			for _, comment := range response.Values {
				createdAt := formatDateToZ(comment.CreatedOn)
				updatedAt := formatDateToZ(comment.UpdatedOn)

				rawBody := strings.TrimSpace(comment.Content.Raw)
				if rawBody == "" {
					commentorUUID := strings.Trim(comment.User.UUID, "{}")
					if commentorUUID == prAuthorMap[prID] {
						rawBody = "📜 _Migrated from Bitbucket: pull request opened without a description._"
					} else {
						rawBody = "📜 _Migrated from Bitbucket: this comment had no content._"
					}
				}
				transformedBody := c.transformCommentBody(rawBody, workspace, repoSlug)
				prNumber := fmt.Sprintf("%d", prID)

				if comment.Inline != nil && comment.Inline.Path != "" {
					if !resolvedSHAs[prID] {
						shortSHA := prCommitMap[prID]
						fullSHA, err := c.GetFullCommitSHA(workspace, repoSlug, shortSHA)
						if err != nil {
							c.logger.Warn("Failed to resolve full SHA for PR",
								zap.Int("pr_id", prID),
								zap.String("short_sha", shortSHA),
								zap.Error(err))
							fullSHA = shortSHA
						}
						prCommitMap[prID] = fullSHA
						resolvedSHAs[prID] = true

					}

					lineNumber := 1
					if comment.Inline.To != nil {
						lineNumber = *comment.Inline.To
					} else if comment.Inline.From != nil {
						lineNumber = *comment.Inline.From
					}

					// Create a unique thread identifier based on the file path and line number
					// rather than the comment ID
					threadKey := fmt.Sprintf("%s-%s-%d", workspace, comment.Inline.Path, lineNumber)
					threadId := fmt.Sprintf("thread-%s", HashString(threadKey))

					// Generate stable comment ID
					commentId := fmt.Sprintf("%d", comment.ID)

					// Handle parent-child relationship
					var inReplyTo *string
					var reviewId string

					if comment.Parent != nil {
						// This is a reply - use parent's ID for the review ID
						parentId := fmt.Sprintf("%d", comment.Parent.ID)
						inReplyTo = &parentId
						reviewId = fmt.Sprintf("review-%d", comment.Parent.ID)
					} else {
						// This is a top-level comment - use its own ID for the review ID
						reviewId = fmt.Sprintf("review-%d", comment.ID)
					}

					commentURL := formatURL("pr_review_comment", workspace, repoSlug, prNumber, commentId)
					reviewURL := formatURL("pr_review", workspace, repoSlug, prNumber, reviewId)
					threadURL := formatURL("pr_review_thread", workspace, repoSlug, prNumber, threadId)
					prFullURL := formatURL("pr", workspace, repoSlug, prNumber)
					userURL := c.resolveUserURL(workspace, comment.User.UUID, comment.User.Nickname, comment.User.DisplayName)
					commitSHA := prCommitMap[prID]

					// Create diff hunk
					diffHunk := fmt.Sprintf("@@ -0,0 +1,%d @@\n+%s", lineNumber, transformedBody)

					// Create review comment with correct format
					reviewComment := data.PullRequestReviewComment{
						Type:                    "pull_request_review_comment",
						URL:                     commentURL,
						PullRequest:             prFullURL,
						PullRequestReview:       reviewURL,
						PullRequestReviewThread: threadURL,
						User:                    userURL,
						CommitID:                commitSHA,
						OriginalCommitId:        commitSHA,
						Path:                    comment.Inline.Path,
						Position:                lineNumber,
						OriginalPosition:        lineNumber,
						Body:                    transformedBody,
						CreatedAt:               createdAt,
						UpdatedAt:               updatedAt,
						Formatter:               "markdown",
						DiffHunk:                diffHunk,
						State:                   1, // GEI integer state for an active review comment (not the same schema as pull_request_reviews string state)
						InReplyTo:               inReplyTo,
						Reactions:               []string{},
						SubjectType:             "line",
					}

					reviewComments = append(reviewComments, reviewComment)
				} else {
					commentURL := formatURL("issue_comment", workspace, repoSlug, prNumber, comment.ID)
					prURL := formatURL("pr", workspace, repoSlug, prNumber)
					userURL := c.resolveUserURL(workspace, comment.User.UUID, comment.User.Nickname, comment.User.DisplayName)

					regularComment := data.IssueComment{
						Type:        "issue_comment",
						URL:         commentURL,
						User:        userURL,
						Body:        transformedBody,
						CreatedAt:   createdAt,
						Formatter:   "markdown",
						Reactions:   []string{},
						PullRequest: prURL,
					}

					regularComments = append(regularComments, regularComment)
				}
			}

			hasMore = response.Next != ""
			if hasMore {
				page++
			}
		}
	}

	c.logger.Info("Pull request comments fetched",
		zap.Int("regular_comments", len(regularComments)),
		zap.Int("review_comments", len(reviewComments)),
		zap.Int("failed_prs", failedPRs))

	return regularComments, reviewComments, nil
}


// GetPullRequestApprovals fetches approval reviews and builds a migration
// summary comment for each PR.  Both are returned to the caller so they can be
// written to the appropriate archive files.
//
// reviews        → pull_request_reviews_000001.json  (COMMENTED state)
// summaryComments → issue_comments_000001.json        (migration summary)
func (c *Client) GetPullRequestApprovals(workspace, repoSlug string, pullRequests []data.PullRequest) (
	reviews []map[string]interface{},
	summaryComments []data.IssueComment,
	err error,
) {
	c.logger.Info("Fetching pull request approvals via PR detail endpoint")

	for _, pr := range pullRequests {
		parts := strings.Split(pr.URL, "/")
		if len(parts) == 0 {
			continue
		}
		prNumber := parts[len(parts)-1]

		// ── Primary: fetch the full PR detail — this includes participants ─────
		// The list endpoint omits participants; the detail endpoint includes them.
		endpoint := fmt.Sprintf("repositories/%s/%s/pullrequests/%s",
			workspace, repoSlug, prNumber)

		var prDetail data.BitbucketPR
		if fetchErr := c.makeRequest("GET", endpoint, &prDetail); fetchErr != nil {
			c.logger.Warn("Failed to fetch PR detail for approval data, will try cache",
				zap.String("pr", prNumber),
				zap.Error(fetchErr))
		} else {
			c.logger.Debug("PR detail participants",
				zap.String("pr", prNumber),
				zap.Int("count", len(prDetail.Participants)))

			for _, p := range prDetail.Participants {
				c.logger.Debug("Participant",
					zap.String("pr", prNumber),
					zap.String("user", p.User.UUID),
					zap.Bool("approved", p.Approved),
					zap.String("state", p.State))
				if !p.Approved {
					continue
				}
				review := c.buildApprovalReview(workspace, repoSlug, prNumber, pr, p)
				reviews = append(reviews, review)
			}

			// Build the migration summary comment using the full PR detail.
			summary := c.buildMigrationSummaryComment(
				workspace, repoSlug, prNumber,
				pr, prDetail.Author.DisplayName, prDetail.Participants,
			)
			summaryComments = append(summaryComments, summary)
			continue
		}

		// ── Fallback: embedded participants cached during GetPullRequests ──────
		// Only reached when the PR detail fetch above failed entirely.
		cached, hasCached := c.prParticipantsCache[pr.URL]
		if !hasCached {
			c.logger.Debug("No participants available for PR (detail fetch failed, no cache)",
				zap.String("pr", prNumber))
		} else {
			c.logger.Debug("Using cached embedded participants as fallback",
				zap.String("pr", prNumber),
				zap.Int("count", len(cached)))
			for _, p := range cached {
				if !p.Approved {
					continue
				}
				review := c.buildApprovalReview(workspace, repoSlug, prNumber, pr, p)
				reviews = append(reviews, review)
			}
		}

		// For the summary comment in the fallback path, use the UUID extracted
		// from pr.User as the author display name (best effort).
		authorParts := strings.Split(pr.User, "/")
		authorName := authorParts[len(authorParts)-1]
		summary := c.buildMigrationSummaryComment(
			workspace, repoSlug, prNumber,
			pr, authorName, cached,
		)
		summaryComments = append(summaryComments, summary)
	}

	c.logger.Info("Pull request approvals fetched",
		zap.Int("reviews", len(reviews)),
		zap.Int("summary_comments", len(summaryComments)))
	return reviews, summaryComments, nil
}

// approvalHashID derives a stable decimal numeric ID from a seed string using
// FNV-32a, producing an integer that looks like a real Bitbucket record ID.
// GEI may parse URL fragments to extract numeric identifiers, so approval
// review/comment/thread URLs must use the same format as real IDs.
func approvalHashID(seed string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(seed))
	return h.Sum32()
}

// buildApprovalReview converts a single approved Bitbucket participant into the
// GitHub-format pull_request_review map expected by the GEI importer.
func (c *Client) buildApprovalReview(workspace, repoSlug, prNumber string,
	pr data.PullRequest, p data.BitbucketParticipant) map[string]interface{} {

	userURL := c.resolveUserURL(workspace, p.User.UUID, p.User.Nickname, p.User.DisplayName)
	prURL := formatURL("pr", workspace, repoSlug, prNumber)
	uuid := strings.Trim(p.User.UUID, "{}")

	// Use the "review-{numericID}" prefix format that createReviews uses for inline
	// comment reviews.  Real inline reviews produce URLs like:
	//   #pullrequestreview-review-57304971
	// GEI may only recognise pull_request_review entries whose URL fragment matches
	// this pattern; entries with just a bare numeric ID are silently dropped.
	reviewNumID := approvalHashID(fmt.Sprintf("approval-review-%s-%s-%s-%s", workspace, repoSlug, prNumber, uuid))
	reviewURL := formatURL("pr_review", workspace, repoSlug, prNumber, fmt.Sprintf("review-%d", reviewNumID))

	participatedAt := p.ParticipatedOn
	if participatedAt == "" {
		if pr.MergedAt != nil && *pr.MergedAt != "" {
			participatedAt = *pr.MergedAt
		} else {
			participatedAt = pr.CreatedAt
		}
	} else {
		participatedAt = formatDateToZ(participatedAt)
	}

	return map[string]interface{}{
		"type":         "pull_request_review",
		"url":          reviewURL,
		"pull_request": prURL,
		"user":         userURL,
		"body":         "📜 _Migrated from Bitbucket: this PR was approved before migration to GitHub._",
		"head_sha":     pr.Head.SHA,
		"formatter":    "markdown",
		// GEI integer state for pull_request_review.
		// 1=COMMENTED is the only state GEI accepts via archive import.
		// States 2 and 3 (APPROVED, CHANGES_REQUESTED) are silently dropped — likely
		// an intentional GEI policy to prevent bypassing branch protection requirements.
		"state":        1,
		"reactions":    []interface{}{},
		"created_at":   participatedAt,
		"submitted_at": participatedAt,
	}
}


// formatDateOnly returns just the YYYY-MM-DD portion of any timestamp that
// formatDateToZ can parse, making it safe to use with Bitbucket or GEI dates.
func formatDateOnly(ts string) string {
	normalised := formatDateToZ(ts)
	if len(normalised) >= 10 {
		return normalised[:10]
	}
	if len(ts) >= 10 {
		return ts[:10] // best-effort fallback
	}
	return ts
}

// buildMigrationSummaryComment produces an issue_comment that summarises a
// migrated pull request: who opened it, who approved or requested changes, and
// when it was closed or merged.
//
// The comment is attributed to the workspace user URL so it appears under a
// clearly identifiable migration mannequin rather than any real participant.
// Once GEI has created the mannequin it can be reclaimed or left as-is.
func (c *Client) buildMigrationSummaryComment(
	workspace, repoSlug, prNumber string,
	pr data.PullRequest,
	authorDisplayName string,
	participants []data.BitbucketParticipant,
) data.IssueComment {

	openDate := formatDateOnly(pr.CreatedAt)

	// Collect approval and needs-work participants, skipping the PR author.
	var approvers []string
	var changesRequested []string
	for _, p := range participants {
		if p.Role == "AUTHOR" {
			continue
		}
		name := p.User.DisplayName
		if name == "" {
			name = strings.Trim(p.User.UUID, "{}")
		}
		if p.Approved {
			date := formatDateOnly(p.ParticipatedOn)
			if date != "" {
				approvers = append(approvers, fmt.Sprintf("%s (%s)", name, date))
			} else {
				approvers = append(approvers, name)
			}
		} else if p.State == "changes_requested" || p.State == "needs_work" {
			changesRequested = append(changesRequested, name)
		}
	}

	// Build the comment body.  Two trailing spaces force a GitHub line-break.
	var lines []string
	lines = append(lines, "📜 **Bitbucket Pull Request Migration Summary**\n")

	author := authorDisplayName
	if author == "" {
		// Extract UUID from URL as a last resort
		parts := strings.Split(pr.User, "/")
		author = parts[len(parts)-1]
	}
	lines = append(lines, fmt.Sprintf("**Opened:** %s by %s", openDate, author))

	if len(approvers) > 0 {
		lines = append(lines, fmt.Sprintf("**Approved by:** %s", strings.Join(approvers, ", ")))
	} else {
		lines = append(lines, "**Approved by:** _(none)_")
	}
	if len(changesRequested) > 0 {
		lines = append(lines, fmt.Sprintf("**Changes requested by:** %s", strings.Join(changesRequested, ", ")))
	}

	if pr.MergedAt != nil && *pr.MergedAt != "" {
		lines = append(lines, fmt.Sprintf("**Merged:** %s", formatDateOnly(*pr.MergedAt)))
	} else if pr.ClosedAt != nil && *pr.ClosedAt != "" {
		lines = append(lines, fmt.Sprintf("**Closed:** %s", formatDateOnly(*pr.ClosedAt)))
	} else {
		lines = append(lines, "**Status:** Open at time of migration")
	}

	body := strings.Join(lines, "  \n")

	// Pin the comment 1 second after close/merge so it appears at the bottom
	// of the PR timeline.
	commentTime := pr.CreatedAt
	if pr.MergedAt != nil && *pr.MergedAt != "" {
		commentTime = *pr.MergedAt
	} else if pr.ClosedAt != nil && *pr.ClosedAt != "" {
		commentTime = *pr.ClosedAt
	}
	if t, parseErr := time.Parse(time.RFC3339, commentTime); parseErr == nil {
		commentTime = formatDateToZ(t.Add(time.Second).Format(time.RFC3339))
	}

	// Stable unique ID for the comment URL so re-runs don't produce duplicates.
	seed := fmt.Sprintf("summary-%s-%s-%s", workspace, repoSlug, prNumber)
	commentID := approvalHashID(seed)

	prURL := formatURL("pr", workspace, repoSlug, prNumber)
	commentURL := formatURL("issue_comment", workspace, repoSlug, prNumber, commentID)

	// Attribute to the workspace system user — this becomes a mannequin
	// clearly named after the workspace, marking it as a migration artefact.
	userURL := fmt.Sprintf("https://bitbucket.org/%s", workspace)

	return data.IssueComment{
		Type:        "issue_comment",
		URL:         commentURL,
		User:        userURL,
		Body:        body,
		CreatedAt:   commentTime,
		Formatter:   "markdown",
		Reactions:   []string{},
		PullRequest: prURL,
	}
}

// getFileDiffAnchor is retained for potential future use but is no longer called
// getNearestCommitSHA returns the full 40-char SHA of the most recent commit
// on branchName that was created at or before beforeDate (RFC3339 / ISO-8601).
// It uses the bare git repo already cloned into the export directory.
// Returns "" if the repo is not found, the branch has no commits before that
// date, or any git command fails.
func (c *Client) getNearestCommitSHA(workspace, repoSlug, branchName, beforeDate string) string {
	repoPath := filepath.Join(c.exportDir, "repositories", workspace, repoSlug+".git")
	if _, err := os.Stat(repoPath); err != nil {
		c.logger.Debug("getNearestCommitSHA: local git repo not found",
			zap.String("path", repoPath))
		return ""
	}

	// Normalise the date to a format git accepts (YYYY-MM-DD is fine).
	dateStr := formatDateOnly(beforeDate)

	// Try several ref forms — the bare clone may use heads/ or remotes/origin/.
	refs := []string{
		"refs/heads/" + branchName,
		"refs/remotes/origin/" + branchName,
		branchName,
	}

	for _, ref := range refs {
		out, err := exec.Command(
			"git", "--git-dir", repoPath,
			"log", "--before="+dateStr, "--format=%H", ref,
		).Output()
		if err != nil || len(strings.TrimSpace(string(out))) == 0 {
			continue
		}
		// First line is the most recent commit on or before the date.
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 0 && len(lines[0]) == 40 {
			return lines[0]
		}
	}

	// No commit predates the PR's open date — the clone's history doesn't reach
	// that far back (objects GC'd on Bitbucket's side before the clone was made).
	// Fall back to the oldest available commit on the branch: it is at least the
	// closest surviving ancestor, and GEI will accept any valid 40-char SHA.
	c.logger.Debug("getNearestCommitSHA: no commit found before date, trying oldest commit on branch",
		zap.String("branch", branchName),
		zap.String("before", dateStr))

	for _, ref := range refs {
		out, err := exec.Command(
			"git", "--git-dir", repoPath,
			"log", "--reverse", "--format=%H", ref,
		).Output()
		if err != nil || len(strings.TrimSpace(string(out))) == 0 {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 0 && len(lines[0]) == 40 {
			c.logger.Debug("getNearestCommitSHA: using oldest available commit as fallback",
				zap.String("branch", branchName),
				zap.String("sha", lines[0]))
			return lines[0]
		}
	}

	c.logger.Debug("getNearestCommitSHA: branch has no resolvable commits",
		zap.String("branch", branchName))
	return ""
}

// by the approval flow (COMMENTED reviews with a non-empty body import without
// a linked comment).
func (c *Client) getFileDiffAnchor(workspace, repoSlug, baseSHA, headSHA, filePath string) (position int, diffHunk string) {
	// Sensible fallback in case anything goes wrong.
	position = 1
	diffHunk = "@@ -0,0 +1,1 @@\n+Approved"

	if baseSHA == "" || headSHA == "" {
		c.logger.Debug("getFileDiffAnchor: missing SHA, using fallback",
			zap.String("base", baseSHA), zap.String("head", headSHA))
		return
	}

	repoPath := filepath.Join(c.exportDir, "repositories", workspace, repoSlug+".git")
	if _, err := os.Stat(repoPath); err != nil {
		c.logger.Debug("getFileDiffAnchor: local git repo not found, using fallback",
			zap.String("path", repoPath))
		return
	}

	out, err := exec.Command(
		"git", "--git-dir", repoPath,
		"diff", "--unified=3", "--no-color", baseSHA, headSHA,
	).Output()
	if err != nil {
		c.logger.Warn("getFileDiffAnchor: git diff failed, using fallback",
			zap.String("base", baseSHA), zap.String("head", headSHA),
			zap.Error(err))
		return
	}

	// Walk the combined diff counting positions until we find our target file.
	//
	// Position counter rules (mirrors GitHub's definition):
	//   - DO count    : @@ hunk headers, + / - / space content lines
	//   - DO NOT count: diff --git, index, ---, +++, mode/rename/binary headers
	pos := 0
	inTargetFile := false
	foundHunk := false
	var hunkLines []string

	for _, line := range strings.Split(string(out), "\n") {
		// ── Per-file separator lines ─────────────────────────────────────
		if strings.HasPrefix(line, "diff --git ") {
			if foundHunk {
				// We already collected what we need; stop.
				break
			}
			// b/path/to/file or b/"path/to/file" (spaces quoted)
			inTargetFile = strings.Contains(line, " b/"+filePath)
			continue
		}

		// These header lines don't count as positions.
		if strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "--- ") ||
			strings.HasPrefix(line, "+++ ") ||
			strings.HasPrefix(line, "new file mode") ||
			strings.HasPrefix(line, "deleted file mode") ||
			strings.HasPrefix(line, "old mode") ||
			strings.HasPrefix(line, "new mode") ||
			strings.HasPrefix(line, "similarity index") ||
			strings.HasPrefix(line, "rename from") ||
			strings.HasPrefix(line, "rename to") ||
			strings.HasPrefix(line, "Binary files") {
			continue
		}

		// ── Hunk header ──────────────────────────────────────────────────
		if strings.HasPrefix(line, "@@") {
			if foundHunk {
				// Second hunk of target file – we've collected enough context.
				break
			}
			pos++
			if inTargetFile {
				foundHunk = true
				position = pos
				hunkLines = append(hunkLines, line)
			}
			continue
		}

		// ── Content line ─────────────────────────────────────────────────
		if len(line) > 0 && (line[0] == '+' || line[0] == '-' || line[0] == ' ') {
			pos++
			// Collect up to 4 content lines after the hunk header for context.
			if foundHunk && len(hunkLines) < 5 {
				hunkLines = append(hunkLines, line)
			}
		}
	}

	if len(hunkLines) > 0 {
		diffHunk = strings.Join(hunkLines, "\n")
		c.logger.Debug("getFileDiffAnchor: computed real diff anchor",
			zap.String("filePath", filePath),
			zap.Int("position", position),
			zap.String("diffHunk", diffHunk))
	} else {
		c.logger.Warn("getFileDiffAnchor: file not found in diff, using fallback position",
			zap.String("filePath", filePath),
			zap.String("base", baseSHA),
			zap.String("head", headSHA))
	}
	return
}


func (c *Client) transformCommentBody(body, workspace, repoSlug string) string {
	if body == "" {
		return body
	}

	pattern := fmt.Sprintf("https://bitbucket.org/%s/%s/pull-requests/(\\d+)",
		regexp.QuoteMeta(workspace), regexp.QuoteMeta(repoSlug))
	replacement := fmt.Sprintf("https://bitbucket.org/%s/%s/pull/$1",
		workspace, repoSlug)

	re := regexp.MustCompile(pattern)
	transformedBody := re.ReplaceAllString(body, replacement)

	transformedBody = prNumberPattern.ReplaceAllStringFunc(transformedBody, func(match string) string {
		numStr := match[1:] // Remove the # prefix
		return fmt.Sprintf("[%s](%s)", match, fmt.Sprintf("https://bitbucket.org/%s/%s/pull/%s",
			workspace, repoSlug, numStr))
	})

	return transformedBody
}
