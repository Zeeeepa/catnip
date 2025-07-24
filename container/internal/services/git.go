package services

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/catnip/internal/git"
	"github.com/vanpelt/catnip/internal/models"
)

const (
	defaultWorkspaceDir = "/workspace"
	liveDir             = "/live"
	devRepoPath         = "/live/catnip" // Kept for backwards compatibility
)

// getWorkspaceDir returns the workspace directory, configurable via CATNIP_WORKSPACE_DIR
func getWorkspaceDir() string {
	if dir := os.Getenv("CATNIP_WORKSPACE_DIR"); dir != "" {
		return dir
	}
	return defaultWorkspaceDir
}

// getGitStateDir returns the git state directory based on workspace dir
func getGitStateDir() string {
	return filepath.Join(getWorkspaceDir(), ".git-state")
}

// generateUniqueSessionName generates a unique session name that doesn't already exist as a branch
func (s *GitService) generateUniqueSessionName(repoPath string) string {
	// Use the shared function with GitService's branch checking logic
	return git.GenerateUniqueSessionName(func(name string) bool {
		return s.branchExists(repoPath, name, false)
	})
}

// isCatnipBranch checks if a branch name has a catnip/ prefix
func isCatnipBranch(branchName string) bool {
	return git.IsCatnipBranch(branchName)
}

// cleanupUnusedBranches removes catnip branches that have no commits
func (s *GitService) cleanupUnusedBranches() {
	log.Printf("🧹 Starting cleanup of unused catnip branches...")

	s.mu.RLock()
	repos := make([]*models.Repository, 0, len(s.repositories))
	for _, repo := range s.repositories {
		repos = append(repos, repo)
	}
	s.mu.RUnlock()

	totalDeleted := 0

	for _, repo := range repos {
		// List all branches in the bare repository
		branches, err := s.operations.ListBranches(repo.Path, git.ListBranchesOptions{All: true})
		if err != nil {
			log.Printf("⚠️  Failed to list branches for %s: %v", repo.ID, err)
			continue
		}
		deletedInRepo := 0

		for _, branch := range branches {
			// Clean up branch name
			branchName := strings.TrimSpace(branch)
			branchName = strings.TrimPrefix(branchName, "*")
			branchName = strings.TrimPrefix(branchName, "+")
			branchName = strings.TrimSpace(branchName)
			branchName = strings.TrimPrefix(branchName, "remotes/origin/")

			// Skip if not a catnip branch
			if !isCatnipBranch(branchName) {
				continue
			}

			// Check if branch has any commits different from its parent
			// First, try to find the merge-base with main/master
			var baseRef string
			for _, ref := range []string{"main", "master"} {
				if err := s.operations.ShowRef(repo.Path, ref, git.ShowRefOptions{Verify: true, Quiet: true}); err == nil {
					baseRef = ref
					break
				}
			}

			if baseRef == "" {
				continue // Skip if we can't find a base branch
			}

			// Check if branch exists locally
			if !s.operations.BranchExists(repo.Path, branchName, false) {
				continue // Branch doesn't exist locally
			}

			// Count commits ahead of base
			commitCount, err := s.operations.GetCommitCount(repo.Path, baseRef, branchName)
			if err != nil || commitCount > 0 {
				continue // Skip if there are commits or error parsing
			}

			// Also check if there's an active worktree using this branch
			worktrees, err := s.operations.ListWorktrees(repo.Path)
			if err == nil {
				var skipBranch bool
				for _, wt := range worktrees {
					if wt.Branch == branchName {
						skipBranch = true
						break
					}
				}
				if skipBranch {
					continue // Skip if branch is currently checked out in a worktree
				}
			}

			// Delete the branch (local)
			if err := s.operations.DeleteBranch(repo.Path, branchName, true); err == nil {
				deletedInRepo++
				totalDeleted++
				log.Printf("🗑️  Deleted unused branch: %s in %s", branchName, repo.ID)
			}
		}

		if deletedInRepo > 0 {
			log.Printf("✅ Cleaned up %d unused branches in %s", deletedInRepo, repo.ID)
		}
	}

	if totalDeleted > 0 {
		log.Printf("🧹 Cleanup complete: removed %d unused catnip branches", totalDeleted)
	} else {
		log.Printf("✅ No unused catnip branches found")
	}
}

// cleanupCatnipRefs provides comprehensive cleanup of refs/catnip/ namespace
func (s *GitService) cleanupCatnipRefs() {
	log.Printf("🧹 Starting cleanup of catnip refs namespace...")

	s.mu.RLock()
	repos := make([]*models.Repository, 0, len(s.repositories))
	for _, repo := range s.repositories {
		repos = append(repos, repo)
	}
	s.mu.RUnlock()

	totalDeleted := 0

	for _, repo := range repos {
		// Use git for-each-ref to list all refs/catnip/ references
		output, err := s.operations.ExecuteGit(repo.Path, "for-each-ref", "--format=%(refname)", "refs/catnip/")
		if err != nil {
			log.Printf("⚠️  Failed to list catnip refs for %s: %v", repo.ID, err)
			continue
		}

		if strings.TrimSpace(string(output)) == "" {
			continue // No catnip refs to clean up
		}

		deletedInRepo := 0
		refs := strings.Split(strings.TrimSpace(string(output)), "\n")

		for _, ref := range refs {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}

			// Check if there's an active worktree using this ref
			worktrees, err := s.operations.ListWorktrees(repo.Path)
			if err == nil {
				var skipRef bool
				for _, wt := range worktrees {
					if wt.Branch == ref {
						skipRef = true
						break
					}
				}
				if skipRef {
					continue // Skip if ref is currently checked out in a worktree
				}
			}

			// Delete the ref using update-ref
			if _, err := s.operations.ExecuteGit(repo.Path, "update-ref", "-d", ref); err == nil {
				deletedInRepo++
				totalDeleted++
				log.Printf("🗑️  Deleted catnip ref: %s in %s", ref, repo.ID)
			} else {
				log.Printf("⚠️  Failed to delete catnip ref %s: %v", ref, err)
			}
		}

		if deletedInRepo > 0 {
			log.Printf("✅ Cleaned up %d catnip refs in %s", deletedInRepo, repo.ID)
			// Run garbage collection to clean up unreachable objects
			if err := s.operations.GarbageCollect(repo.Path); err != nil {
				log.Printf("⚠️ Failed to run garbage collection for %s: %v", repo.ID, err)
			}
		}
	}

	if totalDeleted > 0 {
		log.Printf("🧹 Catnip refs cleanup complete: removed %d refs", totalDeleted)
	} else {
		log.Printf("✅ No orphaned catnip refs found")
	}
}

// CleanupAllCatnipRefs provides a comprehensive cleanup that handles both legacy catnip/ branches and new refs/catnip/ refs
func (s *GitService) CleanupAllCatnipRefs() {
	log.Printf("🧹 Starting comprehensive catnip cleanup...")

	// Clean up legacy catnip/ branches first
	s.cleanupUnusedBranches()

	// Then clean up new refs/catnip/ namespace
	s.cleanupCatnipRefs()

	log.Printf("✅ Comprehensive catnip cleanup complete")
}

// GitService manages multiple Git repositories and their worktrees
type GitService struct {
	repositories     map[string]*models.Repository // key: repoID (e.g., "owner/repo")
	worktrees        map[string]*models.Worktree   // key: worktree ID
	operations       git.Operations                // All git operations through this interface
	worktreeService  *WorktreeManager              // Handles all worktree operations (services layer)
	conflictResolver *git.ConflictResolver         // Handles conflict detection/resolution
	githubManager    *git.GitHubManager            // Handles all GitHub CLI operations
	commitSync       *CommitSyncService            // Handles automatic checkpointing and commit sync
	mu               sync.RWMutex
}

// Helper functions for standardized command execution

// Repository type detection helpers
func (s *GitService) isLocalRepo(repoID string) bool {
	return strings.HasPrefix(repoID, "local/")
}

// Helper methods for command execution - using operations interface where possible
func (s *GitService) execCommand(command string, args ...string) *exec.Cmd {
	cmd := exec.Command(command, args...)
	cmd.Env = append(os.Environ(),
		"HOME=/home/catnip",
		"USER=catnip",
	)
	return cmd
}

func (s *GitService) runGitCommand(workingDir string, args ...string) ([]byte, error) {
	return s.operations.ExecuteGit(workingDir, args...)
}

// getSourceRef returns the appropriate source reference for a worktree
func (s *GitService) getSourceRef(worktree *models.Worktree) string {
	if s.isLocalRepo(worktree.RepoID) {
		// For local repos, use the local branch directly since it's the source of truth
		// The live remote can become stale and doesn't represent the current state
		return worktree.SourceBranch
	}
	return fmt.Sprintf("origin/%s", worktree.SourceBranch)
}

// Removed RemoteURLManager - functionality moved to git.URLManager

// PushStrategy defines the strategy for pushing branches (DEPRECATED: use git.PushStrategy)
type PushStrategy struct {
	Branch       string // Branch to push (defaults to worktree.Branch)
	Remote       string // Remote name (defaults to "origin")
	RemoteURL    string // Remote URL (optional, for local repos)
	SyncOnFail   bool   // Whether to sync with upstream on push failure
	SetUpstream  bool   // Whether to set upstream (-u flag)
	ConvertHTTPS bool   // Whether to convert SSH URLs to HTTPS
}

// pushBranch unified push method with strategy pattern
func (s *GitService) pushBranch(worktree *models.Worktree, repo *models.Repository, strategy PushStrategy) error {
	// Convert to git package strategy
	gitStrategy := git.PushStrategy{
		Branch:       strategy.Branch,
		Remote:       strategy.Remote,
		RemoteURL:    strategy.RemoteURL,
		SyncOnFail:   false, // We handle sync retry at this level
		SetUpstream:  strategy.SetUpstream,
		ConvertHTTPS: strategy.ConvertHTTPS,
	}

	// Set defaults
	if gitStrategy.Branch == "" {
		gitStrategy.Branch = worktree.Branch
	}
	if gitStrategy.Remote == "" {
		gitStrategy.Remote = "origin"
	}

	// Execute push using operations
	err := s.operations.PushBranch(worktree.Path, gitStrategy)

	// Handle push failure with sync retry (if requested)
	if err != nil && strategy.SyncOnFail && git.IsPushRejected(err, err.Error()) {
		log.Printf("🔄 Push rejected due to upstream changes, syncing and retrying")

		// Sync with upstream
		if syncErr := s.syncBranchWithUpstream(worktree); syncErr != nil {
			return fmt.Errorf("failed to sync with upstream: %v", syncErr)
		}

		// Retry the push (without sync this time to avoid infinite loop)
		retryStrategy := strategy
		retryStrategy.SyncOnFail = false
		return s.pushBranch(worktree, repo, retryStrategy)
	}

	return err
}

// branchExists checks if a branch exists in a repository with configurable options
func (s *GitService) branchExists(repoPath, branch string, isRemote bool) bool {
	return s.operations.BranchExists(repoPath, branch, isRemote)
}

// getRemoteURL gets the remote URL for a repository
func (s *GitService) getRemoteURL(repoPath string) (string, error) {
	return s.operations.GetRemoteURL(repoPath)
}

// getDefaultBranch gets the default branch from a repository
func (s *GitService) getDefaultBranch(repoPath string) (string, error) {
	return s.operations.GetDefaultBranch(repoPath)
}

// fetchBranch unified fetch method with strategy pattern
func (s *GitService) fetchBranch(repoPath string, strategy git.FetchStrategy) error {
	return s.operations.FetchBranch(repoPath, strategy)
}

// NewGitService creates a new Git service instance
func NewGitService() *GitService {
	fmt.Println("🐛 [DEBUG] NewGitService called - debug logging is active!")
	return NewGitServiceWithOperations(git.NewOperations())
}

// NewGitServiceWithOperations creates a new Git service instance with injectable git operations
func NewGitServiceWithOperations(operations git.Operations) *GitService {
	s := &GitService{
		repositories:     make(map[string]*models.Repository),
		worktrees:        make(map[string]*models.Worktree),
		operations:       operations,
		worktreeService:  NewWorktreeManager(operations),
		conflictResolver: git.NewConflictResolver(operations),
		githubManager:    git.NewGitHubManager(operations),
	}

	// Initialize CommitSync service
	s.commitSync = NewCommitSyncServiceWithOperations(s, operations)

	// Ensure workspace directory exists
	_ = os.MkdirAll(getWorkspaceDir(), 0755)
	_ = os.MkdirAll(getGitStateDir(), 0755)

	// Configure Git to use gh as credential helper if available
	s.configureGitCredentials()

	// Load existing state (repositories and worktrees) from previous sessions
	if err := s.loadState(); err != nil {
		log.Printf("⚠️ Failed to load GitService state: %v", err)
	}

	// Detect and load any local repositories in /live
	s.detectLocalRepos()

	// Clean up unused catnip branches (skip in dev mode to avoid deleting active branches)
	if os.Getenv("CATNIP_DEV") != "true" {
		s.cleanupUnusedBranches()
	} else {
		log.Printf("🔧 Skipping branch cleanup in dev mode")
	}

	// Start CommitSync service for automatic checkpointing
	if err := s.commitSync.Start(); err != nil {
		log.Printf("⚠️ Failed to start CommitSync service: %v", err)
	}

	return s
}

// CheckoutRepository clones a GitHub repository as a bare repo and creates initial worktree
func (s *GitService) CheckoutRepository(org, repo, branch string) (*models.Repository, *models.Worktree, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	repoID := fmt.Sprintf("%s/%s", org, repo)

	// Handle local repo specially
	if s.isLocalRepo(repoID) {
		return s.handleLocalRepoWorktree(repoID, branch)
	}

	repoURL := fmt.Sprintf("https://github.com/%s/%s.git", org, repo)
	repoName := strings.ReplaceAll(repo, "/", "-")
	barePath := filepath.Join(getWorkspaceDir(), fmt.Sprintf("%s.git", repoName))

	// Check if a directory is already mounted at the repo location
	if s.isRepoMounted(getWorkspaceDir(), repoName) {
		return nil, nil, fmt.Errorf("a repository already exists at %s (possibly mounted)",
			filepath.Join(getWorkspaceDir(), repoName))
	}

	// Check if repository already exists in our map
	if existingRepo, exists := s.repositories[repoID]; exists {
		log.Printf("🔄 Repository already loaded, creating new worktree: %s", repoID)
		return s.createWorktreeForExistingRepo(existingRepo, branch)
	}

	// Check if bare repository already exists on disk
	if _, err := os.Stat(barePath); err == nil {
		log.Printf("🔄 Found existing bare repository, loading and creating new worktree: %s", repoID)
		return s.handleExistingRepository(repoID, repoURL, barePath, branch)
	}

	log.Printf("🔄 Cloning new repository: %s", repoID)
	return s.cloneNewRepository(repoID, repoURL, barePath, branch)
}

// isRepoMounted checks if a repo directory is already mounted
func (s *GitService) isRepoMounted(workspaceDir, repoName string) bool {
	potentialMountPath := filepath.Join(workspaceDir, repoName)
	if info, err := os.Stat(potentialMountPath); err == nil && info.IsDir() {
		if _, err := os.Stat(filepath.Join(potentialMountPath, ".git")); err == nil {
			log.Printf("⚠️ Found existing Git repository at %s, skipping checkout", potentialMountPath)
			return true
		}
	}
	return false
}

// handleExistingRepository handles checkout when bare repo already exists
func (s *GitService) handleExistingRepository(repoID, repoURL, barePath, branch string) (*models.Repository, *models.Worktree, error) {
	// Load existing repository if we have state
	var repo *models.Repository
	if existingRepo, exists := s.repositories[repoID]; exists {
		log.Printf("📦 Repository already loaded: %s", repoID)
		repo = existingRepo
	} else {
		// Create repository object for existing bare repo
		defaultBranch, err := s.getDefaultBranch(barePath)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get default branch: %v", err)
		}

		repo = &models.Repository{
			ID:            repoID,
			URL:           repoURL,
			Path:          barePath,
			DefaultBranch: defaultBranch,
			CreatedAt:     time.Now(),
			LastAccessed:  time.Now(),
		}
		s.repositories[repoID] = repo
	}

	// If no branch specified, use default
	if branch == "" {
		branch = repo.DefaultBranch
	}

	// Check if the requested branch exists in the bare repo
	if !s.branchExists(barePath, branch, true) {
		log.Printf("🔄 Branch %s not found, fetching from remote", branch)
		if err := s.fetchBranch(barePath, git.FetchStrategy{
			Branch:         branch,
			Depth:          1,
			UpdateLocalRef: true,
		}); err != nil {
			return nil, nil, fmt.Errorf("failed to fetch branch %s: %v", branch, err)
		}
	}

	// Create new worktree with fun name
	funName := s.generateUniqueSessionName(repo.Path)
	worktree, err := s.createWorktreeInternalForRepo(repo, branch, funName, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create worktree: %v", err)
	}

	_ = s.saveState()
	log.Printf("✅ Worktree created from existing repository: %s", repoID)
	return repo, worktree, nil
}

// cloneNewRepository clones a new bare repository
func (s *GitService) cloneNewRepository(repoID, repoURL, barePath, branch string) (*models.Repository, *models.Worktree, error) {
	// Clone as bare repository with shallow depth
	args := []string{"clone", "--bare", "--depth", "1", "--single-branch"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, repoURL, barePath)

	if _, err := s.runGitCommand("", args...); err != nil {
		return nil, nil, fmt.Errorf("failed to clone repository: %v", err)
	}

	// Get default branch if not specified
	if branch == "" {
		var err error
		branch, err = s.getDefaultBranch(barePath)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get default branch: %v", err)
		}
	}

	// Create repository object
	repository := &models.Repository{
		ID:            repoID,
		URL:           repoURL,
		Path:          barePath,
		DefaultBranch: branch,
		CreatedAt:     time.Now(),
		LastAccessed:  time.Now(),
	}

	s.repositories[repoID] = repository

	// Start background unshallow process for the requested branch
	go s.unshallowRepository(barePath, branch)

	// Create initial worktree with fun name to avoid conflicts with local branches
	funName := s.generateUniqueSessionName(repository.Path)
	worktree, err := s.createWorktreeInternalForRepo(repository, branch, funName, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create initial worktree: %v", err)
	}

	_ = s.saveState()
	log.Printf("✅ Repository cloned successfully: %s", repository.ID)
	return repository, worktree, nil
}

// ListWorktrees returns all worktrees
func (s *GitService) ListWorktrees() []*models.Worktree {
	s.mu.Lock()
	defer s.mu.Unlock()

	worktrees := make([]*models.Worktree, 0, len(s.worktrees))
	hasUpdates := false

	for _, wt := range s.worktrees {
		// Store previous values to detect changes
		prevCommitCount := wt.CommitCount
		prevCommitsBehind := wt.CommitsBehind
		prevSourceBranch := wt.SourceBranch
		prevBranch := wt.Branch
		prevCommitHash := wt.CommitHash

		// Use the services layer's UpdateWorktreeStatus which includes dynamic state detection
		s.worktreeService.UpdateWorktreeStatus(wt, false, s.isLocalRepo(wt.RepoID))

		// Check if any values changed
		if wt.CommitCount != prevCommitCount || wt.CommitsBehind != prevCommitsBehind ||
			wt.SourceBranch != prevSourceBranch || wt.Branch != prevBranch || wt.CommitHash != prevCommitHash {
			hasUpdates = true
		}

		worktrees = append(worktrees, wt)
	}

	// Save state if any worktree was updated
	if hasUpdates {
		_ = s.saveState()
	}

	return worktrees
}

// GetStatus returns the current Git status
func (s *GitService) GetStatus() *models.GitStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return &models.GitStatus{
		Repositories:  s.repositories, // All repositories
		WorktreeCount: len(s.worktrees),
	}
}

// updateCurrentSymlink updates the /workspace/current symlink
func (s *GitService) updateCurrentSymlink(targetPath string) error {
	currentPath := filepath.Join(getWorkspaceDir(), "current")

	// Remove existing symlink if it exists
	os.Remove(currentPath)

	// Create new symlink
	return os.Symlink(targetPath, currentPath)
}

// State persistence

func (s *GitService) saveState() error {
	state := map[string]interface{}{
		"repositories": s.repositories,
		"worktrees":    s.worktrees,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(getGitStateDir(), "state.json"), data, 0644)
}

func (s *GitService) loadState() error {
	data, err := os.ReadFile(filepath.Join(getGitStateDir(), "state.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No state to load
		}
		return err
	}

	var state map[string]json.RawMessage
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	// Load repositories - support both old single repo format and new multi-repo format
	if reposData, exists := state["repositories"]; exists {
		// New multi-repo format
		var repos map[string]*models.Repository
		if err := json.Unmarshal(reposData, &repos); err == nil {
			s.repositories = repos
		}
	} else if repoData, exists := state["repository"]; exists {
		// Old single repo format - migrate to new format
		var repo models.Repository
		if err := json.Unmarshal(repoData, &repo); err == nil {
			s.repositories[repo.ID] = &repo
		}
	}

	// Load worktrees
	if worktreesData, exists := state["worktrees"]; exists {
		var worktrees map[string]*models.Worktree
		if err := json.Unmarshal(worktreesData, &worktrees); err == nil {
			s.worktrees = worktrees
		}
	}

	// Note: No longer loading activeWorktree since we removed single active worktree concept

	return nil
}

// GetDefaultWorktreePath returns the path to the most recently accessed worktree
func (s *GitService) GetDefaultWorktreePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Find most recently accessed worktree
	var mostRecentWorktree *models.Worktree
	for _, wt := range s.worktrees {
		if mostRecentWorktree == nil || wt.LastAccessed.After(mostRecentWorktree.LastAccessed) {
			mostRecentWorktree = wt
		}
	}

	if mostRecentWorktree != nil {
		return mostRecentWorktree.Path
	}

	return getWorkspaceDir() // fallback
}

// configureGitCredentials sets up Git to use gh CLI for GitHub authentication
func (s *GitService) configureGitCredentials() {
	if err := s.githubManager.ConfigureGitCredentials(); err != nil {
		log.Printf("❌ Failed to configure Git credential helper: %v", err)
	} else {
		log.Printf("✅ Git credential helper configured successfully")
	}
}

// ListGitHubRepositories returns a list of GitHub repositories accessible to the user
func (s *GitService) ListGitHubRepositories() ([]map[string]interface{}, error) {
	var repos []map[string]interface{}

	// Add all local repositories
	s.mu.RLock()
	for repoID := range s.repositories {
		if s.isLocalRepo(repoID) {
			// Extract the directory name from the repo ID
			dirName := strings.TrimPrefix(repoID, "local/")
			repos = append(repos, map[string]interface{}{
				"name":        dirName,
				"url":         repoID, // Just use the local repo ID directly
				"private":     false,
				"description": "Local repository (mounted)",
				"fullName":    repoID,
			})
		}
	}
	s.mu.RUnlock()

	// Get GitHub repositories
	githubRepos, err := s.githubManager.ListRepositories()
	if err != nil {
		// If GitHub CLI fails, still return dev repo if it exists
		if len(repos) > 0 {
			return repos, nil
		}
		return nil, fmt.Errorf("failed to list GitHub repositories: %w", err)
	}

	// Transform the GitHub data to match frontend expectations
	for _, repo := range githubRepos {
		repoMap := map[string]interface{}{
			"name":        repo.Name,
			"url":         repo.URL,
			"private":     repo.IsPrivate,
			"description": repo.Description,
		}

		// Add full name for display
		if login, ok := repo.Owner["login"].(string); ok {
			repoMap["fullName"] = fmt.Sprintf("%s/%s", login, repo.Name)
		}

		repos = append(repos, repoMap)
	}

	return repos, nil
}

// detectLocalRepos scans /live for any Git repositories and loads them
func (s *GitService) detectLocalRepos() {
	// Check if /live directory exists
	if _, err := os.Stat(liveDir); os.IsNotExist(err) {
		log.Printf("📁 No /live directory found, skipping local repo detection")
		return
	}

	// Read all entries in /live
	entries, err := os.ReadDir(liveDir)
	if err != nil {
		log.Printf("❌ Failed to read /live directory: %v", err)
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		repoPath := filepath.Join(liveDir, entry.Name())
		gitPath := filepath.Join(repoPath, ".git")

		// Check if it's a git repository
		if _, err := os.Stat(gitPath); os.IsNotExist(err) {
			continue
		}

		log.Printf("🔍 Detected local repository at %s", repoPath)

		// Create repository object
		repoID := fmt.Sprintf("local/%s", entry.Name())
		repo := &models.Repository{
			ID:            repoID,
			URL:           "file://" + repoPath,
			Path:          repoPath,
			DefaultBranch: s.getLocalRepoDefaultBranch(repoPath),
			CreatedAt:     time.Now(),
			LastAccessed:  time.Now(),
		}

		// Add to repositories map
		s.repositories[repoID] = repo

		log.Printf("✅ Local repository loaded: %s", repoID)

		// Check if any worktrees exist for this repo
		if s.shouldCreateInitialWorktree(repoID) {
			log.Printf("🌱 Creating initial worktree for %s", repoID)
			if _, worktree, err := s.handleLocalRepoWorktree(repoID, repo.DefaultBranch); err != nil {
				log.Printf("❌ Failed to create initial worktree for %s: %v", repoID, err)
			} else {
				log.Printf("✅ Initial worktree created: %s", worktree.Name)
			}
		}
	}
}

// shouldCreateInitialWorktree checks if we should create an initial worktree for a repo
func (s *GitService) shouldCreateInitialWorktree(repoID string) bool {
	// Check if any worktrees exist for this repo in /workspace
	dirName := filepath.Base(strings.TrimPrefix(repoID, "local/"))
	repoWorkspaceDir := filepath.Join(getWorkspaceDir(), dirName)

	// Check if the repo workspace directory exists and has any worktrees
	if entries, err := os.ReadDir(repoWorkspaceDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				// Check if this directory is a valid git worktree
				if _, err := os.Stat(filepath.Join(repoWorkspaceDir, entry.Name(), ".git")); err == nil {
					log.Printf("🔍 Found existing worktree for %s: %s", repoID, entry.Name())
					return false
				}
			}
		}
	}

	log.Printf("🔍 No existing worktrees found for %s, will create initial worktree", repoID)
	return true
}

// getLocalRepoDefaultBranch gets the current branch of a local repo
func (s *GitService) getLocalRepoDefaultBranch(repoPath string) string {
	output, err := s.runGitCommand(repoPath, "branch", "--show-current")
	if err != nil {
		log.Printf("⚠️ Could not get current branch for repo at %s, using fallback: main", repoPath)
		return "main"
	}

	branch := strings.TrimSpace(string(output))
	if branch == "" {
		return "main"
	}

	return branch
}

// handleLocalRepoWorktree creates a worktree for any local repo
func (s *GitService) handleLocalRepoWorktree(repoID, branch string) (*models.Repository, *models.Worktree, error) {
	// Get the local repo from repositories map
	localRepo, exists := s.repositories[repoID]
	if !exists {
		return nil, nil, fmt.Errorf("local repository %s not found - it may not be mounted", repoID)
	}

	// If no branch specified, use current branch
	if branch == "" {
		branch = localRepo.DefaultBranch
	}

	// Check if branch exists in the local repo
	if !s.branchExists(localRepo.Path, branch, false) {
		return nil, nil, fmt.Errorf("branch %s does not exist in repository %s", branch, repoID)
	}

	// Create new worktree with fun name
	funName := s.generateUniqueSessionName(localRepo.Path)

	// Create worktree for local repo
	worktree, err := s.createLocalRepoWorktree(localRepo, branch, funName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create worktree for local repo: %v", err)
	}

	// Save state
	_ = s.saveState()

	log.Printf("✅ Local repo worktree created: %s from branch %s", worktree.Name, worktree.SourceBranch)
	return localRepo, worktree, nil
}

// createLocalRepoWorktree creates a worktree for any local repo
func (s *GitService) createLocalRepoWorktree(repo *models.Repository, branch, name string) (*models.Worktree, error) {
	// Use WorktreeManager to create the local worktree
	worktree, err := s.worktreeService.CreateLocalWorktree(repo, branch, name, getWorkspaceDir())
	if err != nil {
		return nil, err
	}

	// Store worktree in service map
	s.worktrees[worktree.ID] = worktree

	// Update current symlink to point to this worktree if it's the first one
	if len(s.worktrees) == 1 {
		_ = s.updateCurrentSymlink(worktree.Path)
	}

	return worktree, nil
}

// getLocalRepoBranches returns the local branches for a local repository
func (s *GitService) getLocalRepoBranches(repoPath string) ([]string, error) {
	return s.operations.GetLocalBranches(repoPath)
}

// GetRepositoryBranches returns the remote branches for a repository
func (s *GitService) GetRepositoryBranches(repoID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	repo, exists := s.repositories[repoID]
	if !exists {
		return nil, fmt.Errorf("repository %s not found", repoID)
	}

	// Handle local repos specially
	if s.isLocalRepo(repoID) {
		return s.operations.GetLocalBranches(repo.Path)
	}

	// For remote repos, use the operations interface
	return s.operations.GetRemoteBranches(repo.Path, repo.DefaultBranch)
}

// DeleteWorktree removes a worktree
func (s *GitService) DeleteWorktree(worktreeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	worktree, exists := s.worktrees[worktreeID]
	if !exists {
		return fmt.Errorf("worktree %s not found", worktreeID)
	}

	// Get repository for worktree deletion
	repo, exists := s.repositories[worktree.RepoID]
	if !exists {
		return fmt.Errorf("repository %s not found", worktree.RepoID)
	}

	// Clean up any active PTY sessions for this worktree (service-specific)
	s.cleanupActiveSessions(worktree.Path)

	// Use WorktreeManager to handle the comprehensive cleanup
	if err := s.worktreeService.DeleteWorktree(repo, worktree); err != nil {
		return err
	}

	// Remove from service memory
	delete(s.worktrees, worktreeID)

	// Save state
	_ = s.saveState()

	return nil
}

// CleanupMergedWorktrees removes worktrees that have been fully merged into their source branch
func (s *GitService) CleanupMergedWorktrees() (int, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var cleanedUp []string
	var errors []error

	log.Printf("🧹 Starting cleanup of merged worktrees, checking %d worktrees", len(s.worktrees))

	for worktreeID, worktree := range s.worktrees {
		log.Printf("🔍 Checking worktree %s: dirty=%v, conflicts=%v, commits_ahead=%d, source=%s",
			worktree.Name, worktree.IsDirty, worktree.HasConflicts, worktree.CommitCount, worktree.SourceBranch)

		// Skip if worktree has uncommitted changes or conflicts
		if worktree.IsDirty {
			log.Printf("⏭️  Skipping cleanup of dirty worktree: %s", worktree.Name)
			continue
		}
		if worktree.HasConflicts {
			log.Printf("⏭️  Skipping cleanup of conflicted worktree: %s", worktree.Name)
			continue
		}

		// Skip if worktree has commits ahead of source
		if worktree.CommitCount > 0 {
			log.Printf("⏭️  Skipping cleanup of worktree with %d commits ahead: %s", worktree.CommitCount, worktree.Name)
			continue
		}

		// Check if the worktree branch exists in the source repo
		repo, exists := s.repositories[worktree.RepoID]
		if !exists {
			continue
		}

		// For local repos, check if the worktree branch no longer exists or if it matches the source branch
		isLocal := s.isLocalRepo(worktree.RepoID)
		var isMerged bool

		if isLocal {
			log.Printf("🔍 Checking local worktree %s: branch=%s, source=%s", worktree.Name, worktree.Branch, worktree.SourceBranch)

			// For local repos, check if the branch exists in the main repo
			// If it doesn't exist, it was likely deleted after merge
			branchExists := s.operations.BranchExists(repo.Path, worktree.Branch, false)

			if !branchExists {
				log.Printf("✅ Branch %s no longer exists in main repo (likely merged and deleted)", worktree.Branch)
				isMerged = true
			} else {
				// Branch still exists, check if it's merged
				branches, err := s.operations.ListBranches(repo.Path, git.ListBranchesOptions{Merged: worktree.SourceBranch})
				if err != nil {
					log.Printf("⚠️ Failed to check merged status for %s: %v", worktree.Name, err)
					continue
				}

				for _, branch := range branches {
					branch = git.CleanBranchName(branch)
					if branch == worktree.Branch {
						isMerged = true
						log.Printf("✅ Found %s in merged branches list", worktree.Branch)
						break
					}
				}
			}
		} else {
			// Regular repo logic (existing code)
			log.Printf("🔍 Checking if branch %s is merged into %s in repo %s", worktree.Branch, worktree.SourceBranch, repo.Path)
			branches, err := s.operations.ListBranches(repo.Path, git.ListBranchesOptions{Merged: worktree.SourceBranch})
			if err != nil {
				log.Printf("⚠️ Failed to check merged status for %s: %v", worktree.Name, err)
				continue
			}

			// Check if our branch appears in the merged branches list
			log.Printf("📋 Merged branches into %s: %d branches found", worktree.SourceBranch, len(branches))

			for _, branch := range branches {
				// Handle both regular branches and worktree branches (marked with +)
				branch = git.CleanBranchName(branch)
				if branch == worktree.Branch {
					isMerged = true
					log.Printf("✅ Found %s in merged branches list", worktree.Branch)
					break
				}
			}
		}

		if !isMerged {
			log.Printf("❌ Branch %s not eligible for cleanup", worktree.Branch)
		}

		if isMerged {
			log.Printf("🧹 Found merged worktree to cleanup: %s", worktree.Name)

			// Use the existing deletion logic but don't hold the mutex
			s.mu.Unlock()
			if cleanupErr := s.DeleteWorktree(worktreeID); cleanupErr != nil {
				errors = append(errors, fmt.Errorf("failed to cleanup worktree %s: %v", worktree.Name, cleanupErr))
			} else {
				cleanedUp = append(cleanedUp, worktree.Name)
			}
			s.mu.Lock()
		}
	}

	if len(cleanedUp) > 0 {
		log.Printf("✅ Cleaned up %d merged worktrees: %s", len(cleanedUp), strings.Join(cleanedUp, ", "))
	}

	if len(errors) > 0 {
		return len(cleanedUp), cleanedUp, fmt.Errorf("cleanup completed with %d errors: %v", len(errors), errors)
	}

	return len(cleanedUp), cleanedUp, nil
}

// cleanupActiveSessions attempts to cleanup any active terminal sessions for this worktree
func (s *GitService) cleanupActiveSessions(worktreePath string) {
	// Kill any processes that might be running in the worktree directory
	// This is a best-effort cleanup
	cmd := s.execCommand("pkill", "-f", worktreePath)
	if err := cmd.Run(); err != nil {
		// Don't log this as an error since it's common for no processes to be found
		log.Printf("ℹ️ No active processes found for worktree path: %s", worktreePath)
	} else {
		log.Printf("✅ Terminated processes for worktree: %s", worktreePath)
	}

	// Also try to cleanup any session directories that might exist
	// Session IDs are typically derived from worktree names
	parts := strings.Split(strings.TrimPrefix(worktreePath, "/workspace/"), "/")
	if len(parts) >= 2 {
		sessionID := fmt.Sprintf("%s/%s", parts[0], parts[1])
		sessionWorkDir := filepath.Join("/workspace", sessionID)

		// If there's a session directory different from the worktree, clean it up too
		if sessionWorkDir != worktreePath {
			if _, err := os.Stat(sessionWorkDir); err == nil {
				if removeErr := os.RemoveAll(sessionWorkDir); removeErr != nil {
					log.Printf("⚠️ Failed to remove session directory %s: %v", sessionWorkDir, removeErr)
				} else {
					log.Printf("✅ Removed session directory: %s", sessionWorkDir)
				}
			}
		}
	}
}

// fetchLatestReference fetches the latest reference for a worktree (shallow fetch for status)
func (s *GitService) fetchLatestReference(worktree *models.Worktree) {
	s.fetchLatestReferenceWithDepth(worktree, true)
}

// fetchFullHistory fetches the full history for a worktree (needed for PR/push operations)
func (s *GitService) fetchFullHistory(worktree *models.Worktree) {
	s.fetchLatestReferenceWithDepth(worktree, false)
}

// fetchLatestReferenceWithDepth fetches the latest reference with optional shallow fetch
func (s *GitService) fetchLatestReferenceWithDepth(worktree *models.Worktree, shallow bool) {
	if s.isLocalRepo(worktree.RepoID) {
		// Get the local repo path
		repo, exists := s.repositories[worktree.RepoID]
		if exists {
			// Local repos: use shallow or full fetch based on need
			if shallow {
				_ = s.fetchLocalBranch(worktree.Path, repo.Path, worktree.SourceBranch)
			} else {
				_ = s.fetchLocalBranchFull(worktree.Path, repo.Path, worktree.SourceBranch)
			}
		}
	} else {
		// Remote repos: use shallow or full fetch based on need
		if shallow {
			_ = s.fetchBranchFast(worktree.Path, worktree.SourceBranch)
		} else {
			_ = s.fetchBranchFull(worktree.Path, worktree.SourceBranch)
		}
	}
}

// fetchBranchFast performs a highly optimized fetch for status updates
func (s *GitService) fetchBranchFast(repoPath, branch string) error {
	return s.operations.FetchBranchFast(repoPath, branch)
}

// fetchBranchFull performs a full fetch for operations that need complete history
func (s *GitService) fetchBranchFull(repoPath, branch string) error {
	return s.operations.FetchBranchFull(repoPath, branch)
}

// fetchLocalBranch performs a highly optimized fetch for local repos
func (s *GitService) fetchLocalBranch(worktreePath, mainRepoPath, branch string) error {
	// First, check if we even need to fetch by comparing commit hashes
	// Get the current commit hash of the remote branch in our worktree
	currentRemoteHash, err := s.runGitCommand(worktreePath, "rev-parse", fmt.Sprintf("live/%s", branch))
	if err != nil {
		// If we don't have the remote ref yet, we need to fetch
		return s.fetchLocalBranchInternal(worktreePath, mainRepoPath, branch)
	}

	// Get the latest commit hash from the main repo
	latestHash, err := s.runGitCommand(mainRepoPath, "rev-parse", branch)
	if err != nil {
		return fmt.Errorf("failed to get latest commit from main repo: %v", err)
	}

	// Compare hashes - if they're the same, no need to fetch
	if strings.TrimSpace(string(currentRemoteHash)) == strings.TrimSpace(string(latestHash)) {
		return nil // No changes, skip fetch
	}

	// Only fetch if there are actual changes
	return s.fetchLocalBranchInternal(worktreePath, mainRepoPath, branch)
}

// fetchLocalBranchInternal performs minimal fetch for local repos when needed
func (s *GitService) fetchLocalBranchInternal(worktreePath, mainRepoPath, branch string) error {
	// Highly optimized fetch for local repos - only fetch the specific branch tip
	args := []string{
		"fetch",
		mainRepoPath,
		fmt.Sprintf("%s:refs/remotes/live/%s", branch, branch),
		"--depth", "1", // Only fetch the latest commit
		"--quiet", // Reduce output noise
	}

	// Execute minimal fetch
	output, err := s.runGitCommand(worktreePath, args...)
	if err != nil {
		return fmt.Errorf("failed to fetch local branch minimal: %v\n%s", err, output)
	}

	return nil
}

// fetchLocalBranchFull performs a full fetch for local repos (needed for PR/push operations)
func (s *GitService) fetchLocalBranchFull(worktreePath, mainRepoPath, branch string) error {
	// First, check if we even need to fetch by comparing commit hashes
	// Get the current commit hash of the remote branch in our worktree
	currentRemoteHash, err := s.runGitCommand(worktreePath, "rev-parse", fmt.Sprintf("live/%s", branch))
	if err != nil {
		// If we don't have the remote ref yet, we need to fetch
		return s.fetchLocalBranchInternalFull(worktreePath, mainRepoPath, branch)
	}

	// Get the latest commit hash from the main repo
	latestHash, err := s.runGitCommand(mainRepoPath, "rev-parse", branch)
	if err != nil {
		return fmt.Errorf("failed to get latest commit from main repo: %v", err)
	}

	// Compare hashes - if they're the same, no need to fetch
	if strings.TrimSpace(string(currentRemoteHash)) == strings.TrimSpace(string(latestHash)) {
		return nil // No changes, skip fetch
	}

	// Only fetch if there are actual changes
	return s.fetchLocalBranchInternalFull(worktreePath, mainRepoPath, branch)
}

// fetchLocalBranchInternalFull performs full fetch for local repos when needed
func (s *GitService) fetchLocalBranchInternalFull(worktreePath, mainRepoPath, branch string) error {
	// Full fetch for local repos - fetch complete history
	args := []string{
		"fetch",
		mainRepoPath,
		fmt.Sprintf("%s:refs/remotes/live/%s", branch, branch),
		"--quiet", // Reduce output noise
		// Note: No --depth flag for full history
	}

	// Execute full fetch
	output, err := s.runGitCommand(worktreePath, args...)
	if err != nil {
		return fmt.Errorf("failed to fetch local branch full: %v\n%s", err, output)
	}

	return nil
}

// SyncWorktree syncs a worktree with its source branch
func (s *GitService) SyncWorktree(worktreeID string, strategy string) error {
	s.mu.RLock()
	worktree, exists := s.worktrees[worktreeID]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("worktree %s not found", worktreeID)
	}

	return s.syncWorktreeInternal(worktree, strategy)
}

// syncWorktreeInternal consolidated sync logic for both local and regular repos
func (s *GitService) syncWorktreeInternal(worktree *models.Worktree, strategy string) error {
	// Ensure we have full history for sync operations
	s.fetchFullHistory(worktree)

	// Get the appropriate source reference (fetch already done by fetchFullHistory)
	sourceRef := s.getSourceRef(worktree)

	// Apply the sync strategy
	if err := s.applySyncStrategy(worktree, strategy, sourceRef); err != nil {
		return err
	}

	// Update worktree status (no need to fetch since we already did fetchFullHistory)
	s.worktreeService.UpdateWorktreeStatus(worktree, false, s.isLocalRepo(worktree.RepoID))

	log.Printf("✅ Synced worktree %s with %s strategy", worktree.Name, strategy)
	return nil
}

// applySyncStrategy applies merge or rebase strategy
func (s *GitService) applySyncStrategy(worktree *models.Worktree, strategy, sourceRef string) error {
	var err error

	switch strategy {
	case "merge":
		err = s.operations.Merge(worktree.Path, sourceRef)
	case "rebase":
		err = s.operations.Rebase(worktree.Path, sourceRef)
	default:
		return fmt.Errorf("unknown sync strategy: %s", strategy)
	}

	if err != nil {
		// Check if this is an uncommitted changes error (not a conflict)
		if s.isUncommittedChangesError(err.Error()) {
			return fmt.Errorf("cannot %s: worktree has staged changes. Please commit or unstage your changes first", strategy)
		}

		// Check if this is a merge conflict
		if s.isMergeConflict(worktree.Path, err.Error()) {
			return s.createMergeConflictError("sync", worktree, err.Error())
		}
		return fmt.Errorf("failed to %s: %v", strategy, err)
	}

	return nil
}

// MergeWorktreeToMain merges a local repo worktree's changes back to the main repository
func (s *GitService) MergeWorktreeToMain(worktreeID string, squash bool) error {
	s.mu.RLock()
	worktree, exists := s.worktrees[worktreeID]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("worktree %s not found", worktreeID)
	}

	// Only works for local repos
	if !s.isLocalRepo(worktree.RepoID) {
		return fmt.Errorf("merge to main only supported for local repositories")
	}

	// Get the local repo
	repo, exists := s.repositories[worktree.RepoID]
	if !exists {
		return fmt.Errorf("local repository %s not found", worktree.RepoID)
	}

	log.Printf("🔄 Merging worktree %s back to main repository", worktree.Name)

	// Ensure we have full history for merge operations
	s.fetchFullHistory(worktree)

	// First, push the worktree branch to the main repo
	output, err := s.runGitCommand(worktree.Path, "push", repo.Path, fmt.Sprintf("%s:%s", worktree.Branch, worktree.Branch))
	if err != nil {
		return fmt.Errorf("failed to push worktree branch to main repo: %v\n%s", err, output)
	}

	// Switch to the source branch in main repo and merge
	output, err = s.runGitCommand(repo.Path, "checkout", worktree.SourceBranch)
	if err != nil {
		return fmt.Errorf("failed to checkout source branch in main repo: %v\n%s", err, output)
	}

	// Merge the worktree branch
	var mergeArgs []string
	if squash {
		mergeArgs = []string{"merge", worktree.Branch, "--squash"}
	} else {
		mergeArgs = []string{"merge", worktree.Branch, "--no-ff", "-m", fmt.Sprintf("Merge branch '%s' from worktree", worktree.Branch)}
	}
	output, err = s.runGitCommand(repo.Path, mergeArgs...)
	if err != nil {
		// Check if this is a merge conflict
		if s.isMergeConflict(repo.Path, string(output)) {
			return s.createMergeConflictError("merge", worktree, string(output))
		}
		return fmt.Errorf("failed to merge worktree branch: %v\n%s", err, output)
	}

	// For squash merges, we need to commit the staged changes
	if squash {
		output, err = s.runGitCommand(repo.Path, "commit", "-m", fmt.Sprintf("Squash merge branch '%s' from worktree", worktree.Branch))
		if err != nil {
			return fmt.Errorf("failed to commit squash merge: %v\n%s", err, output)
		}
	}

	// Delete the feature branch from main repo (cleanup)
	_ = s.operations.DeleteBranch(repo.Path, worktree.Branch, false) // Ignore errors - branch might be in use

	// Get the new commit hash from the main branch after merge
	if newCommitHash, err := s.operations.GetCommitHash(repo.Path, "HEAD"); err != nil {
		log.Printf("⚠️  Failed to get new commit hash after merge: %v", err)
	} else {
		// Update the worktree's commit hash to the new merge point
		s.mu.Lock()
		worktree.CommitHash = newCommitHash
		s.mu.Unlock()
		log.Printf("📝 Updated worktree %s CommitHash to %s", worktree.Name, newCommitHash)
	}

	log.Printf("✅ Merged worktree %s to main repository", worktree.Name)
	return nil
}

// CreateWorktreePreview creates a preview branch in the main repo for viewing changes outside container
func (s *GitService) CreateWorktreePreview(worktreeID string) error {
	s.mu.RLock()
	worktree, exists := s.worktrees[worktreeID]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("worktree %s not found", worktreeID)
	}

	// Only works for local repos
	if !s.isLocalRepo(worktree.RepoID) {
		return fmt.Errorf("preview only supported for local repositories")
	}

	// Get the local repo
	repo, exists := s.repositories[worktree.RepoID]
	if !exists {
		return fmt.Errorf("local repository %s not found", worktree.RepoID)
	}

	previewBranchName := fmt.Sprintf("catnip/%s", git.ExtractWorkspaceName(worktree.Branch))
	log.Printf("🔍 Creating preview branch %s for worktree %s", previewBranchName, worktree.Name)

	// Check if there are uncommitted changes (staged, unstaged, or untracked)
	hasUncommittedChanges, err := s.hasUncommittedChanges(worktree.Path)
	if err != nil {
		return fmt.Errorf("failed to check for uncommitted changes: %v", err)
	}

	var tempCommitHash string
	if hasUncommittedChanges {
		// Create a temporary commit with all uncommitted changes
		tempCommitHash, err = s.createTemporaryCommit(worktree.Path)
		if err != nil {
			return fmt.Errorf("failed to create temporary commit: %v", err)
		}
		defer func() {
			// Reset to remove the temporary commit after pushing
			if tempCommitHash != "" {
				_, _ = s.runGitCommand(worktree.Path, "reset", "--mixed", "HEAD~1")
			}
		}()
	}

	// Check if preview branch already exists and handle accordingly
	shouldForceUpdate, err := s.shouldForceUpdatePreviewBranch(repo.Path, previewBranchName)
	if err != nil {
		return fmt.Errorf("failed to check preview branch status: %v", err)
	}

	// Push the worktree branch to a preview branch in main repo
	pushArgs := []string{"push"}
	if shouldForceUpdate {
		pushArgs = append(pushArgs, "--force")
		log.Printf("🔄 Updating existing preview branch %s", previewBranchName)
	}
	pushArgs = append(pushArgs, repo.Path, fmt.Sprintf("%s:refs/heads/%s", worktree.Branch, previewBranchName))

	output, err := s.runGitCommand(worktree.Path, pushArgs...)
	if err != nil {
		return fmt.Errorf("failed to create preview branch: %v\n%s", err, output)
	}

	action := "created"
	if shouldForceUpdate {
		action = "updated"
	}

	if hasUncommittedChanges {
		log.Printf("✅ Preview branch %s %s with uncommitted changes - you can now checkout this branch outside the container", previewBranchName, action)
	} else {
		log.Printf("✅ Preview branch %s %s - you can now checkout this branch outside the container", previewBranchName, action)
	}
	return nil
}

// shouldForceUpdatePreviewBranch determines if we should force-update an existing preview branch
func (s *GitService) shouldForceUpdatePreviewBranch(repoPath, previewBranchName string) (bool, error) {
	// Check if the preview branch exists
	if _, err := s.runGitCommand(repoPath, "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/heads/%s", previewBranchName)); err != nil {
		// Branch doesn't exist, safe to create
		return false, nil
	}

	// Branch exists - always force update preview branches since they should reflect latest worktree state
	output, err := s.runGitCommand(repoPath, "log", "-1", "--pretty=format:%s", previewBranchName)
	if err != nil {
		return false, fmt.Errorf("failed to get last commit message: %v", err)
	}

	lastCommitMessage := strings.TrimSpace(string(output))
	log.Printf("🔄 Found existing preview branch %s with commit: '%s' - will force update", previewBranchName, lastCommitMessage)
	return true, nil
}

// hasUncommittedChanges checks if the worktree has any uncommitted changes
func (s *GitService) hasUncommittedChanges(worktreePath string) (bool, error) {
	return s.operations.HasUncommittedChanges(worktreePath)
}

// createTemporaryCommit creates a temporary commit with all uncommitted changes
func (s *GitService) createTemporaryCommit(worktreePath string) (string, error) {
	// Add all changes (staged, unstaged, and untracked)
	if output, err := s.runGitCommand(worktreePath, "add", "."); err != nil {
		return "", fmt.Errorf("failed to stage changes: %v\n%s", err, output)
	}

	// Create the commit
	if output, err := s.runGitCommand(worktreePath, "commit", "-m", "Preview: Include all uncommitted changes"); err != nil {
		return "", fmt.Errorf("failed to create temporary commit: %v\n%s", err, output)
	}

	// Get the commit hash
	commitHash, err := s.operations.GetCommitHash(worktreePath, "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to get commit hash: %v", err)
	}
	log.Printf("📝 Created temporary commit %s with uncommitted changes", commitHash[:8])
	return commitHash, nil
}

// revertTemporaryCommit reverts a temporary commit by resetting HEAD~1
func (s *GitService) revertTemporaryCommit(worktreePath, commitHash string) {
	if commitHash != "" {
		_ = s.operations.ResetMixed(worktreePath, "HEAD~1")
	}
}

// isMergeConflict checks if the git command output indicates a merge conflict
func (s *GitService) isMergeConflict(repoPath, output string) bool {
	return s.conflictResolver.IsMergeConflict(repoPath, output)
}

// isUncommittedChangesError checks if the error is due to staged/uncommitted changes
func (s *GitService) isUncommittedChangesError(output string) bool {
	uncommittedIndicators := []string{
		"Your index contains uncommitted changes",
		"cannot rebase: Your index contains uncommitted changes",
		"Please commit or stash them",
	}

	for _, indicator := range uncommittedIndicators {
		if strings.Contains(output, indicator) {
			return true
		}
	}
	return false
}

// createMergeConflictError creates a detailed merge conflict error
func (s *GitService) createMergeConflictError(operation string, worktree *models.Worktree, output string) *models.MergeConflictError {
	return s.conflictResolver.CreateMergeConflictError(operation, worktree.Name, worktree.Path, output)
}

// CheckSyncConflicts checks if syncing a worktree would cause merge conflicts
func (s *GitService) CheckSyncConflicts(worktreeID string) (*models.MergeConflictError, error) {
	s.mu.RLock()
	worktree, exists := s.worktrees[worktreeID]
	s.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("worktree %s not found", worktreeID)
	}

	// Ensure we have full history for accurate conflict detection
	s.fetchFullHistory(worktree)

	// Get the appropriate source reference
	sourceRef := s.getSourceRef(worktree)

	return s.conflictResolver.CheckSyncConflicts(worktree.Path, sourceRef)
}

// CheckMergeConflicts checks if merging a worktree to main would cause conflicts
func (s *GitService) CheckMergeConflicts(worktreeID string) (*models.MergeConflictError, error) {
	s.mu.RLock()
	worktree, exists := s.worktrees[worktreeID]
	s.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("worktree %s not found", worktreeID)
	}

	// Only works for local repos
	if !s.isLocalRepo(worktree.RepoID) {
		return nil, fmt.Errorf("merge conflict check only supported for local repositories")
	}

	// Get the local repo
	repo, exists := s.repositories[worktree.RepoID]
	if !exists {
		return nil, fmt.Errorf("local repository %s not found", worktree.RepoID)
	}

	return s.conflictResolver.CheckMergeConflicts(repo.Path, worktree.Path, worktree.Branch, worktree.SourceBranch, worktree.Name)
}

// Stop stops the Git service
func (s *GitService) Stop() {
	// Stop CommitSync service
	if s.commitSync != nil {
		s.commitSync.Stop()
	}
}

// GitAddCommitGetHash performs git add, commit, and returns the commit hash
// Returns empty string if not a git repository or no changes to commit
func (s *GitService) GitAddCommitGetHash(workspaceDir, message string) (string, error) {
	// Check if it's a git repository
	if !s.operations.IsGitRepository(workspaceDir) {
		log.Printf("📂 Not a git repository, skipping git operations for: %s", workspaceDir)
		return "", nil
	}

	// Stage all changes
	if output, err := s.runGitCommand(workspaceDir, "add", "."); err != nil {
		return "", fmt.Errorf("git add failed: %v, output: %s", err, string(output))
	}

	// Check if there are staged changes to commit
	if _, err := s.runGitCommand(workspaceDir, "diff", "--cached", "--quiet"); err == nil {
		return "", nil
	}

	// Commit with the message
	if output, err := s.runGitCommand(workspaceDir, "commit", "-m", message, "-n"); err != nil {
		return "", fmt.Errorf("git commit failed: %v, output: %s", err, string(output))
	}

	// Get the commit hash
	output, err := s.runGitCommand(workspaceDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse failed: %v", err)
	}

	hash := strings.TrimSpace(string(output))
	return hash, nil
}

// createWorktreeForExistingRepo creates a worktree for an already loaded repository
func (s *GitService) createWorktreeForExistingRepo(repo *models.Repository, branch string) (*models.Repository, *models.Worktree, error) {
	// If no branch specified, use default
	if branch == "" {
		branch = repo.DefaultBranch
	}

	// Handle local repos specially (they don't have a bare repo)
	if s.isLocalRepo(repo.ID) {
		return s.handleLocalRepoWorktree(repo.ID, branch)
	}

	// Always fetch the latest state for checkout operations (full history)
	log.Printf("🔄 Fetching latest state for branch %s", branch)
	if err := s.fetchBranch(repo.Path, git.FetchStrategy{
		Branch:         branch,
		UpdateLocalRef: true,
	}); err != nil {
		// If fetch fails, check if branch exists locally and proceed if so
		if !s.branchExists(repo.Path, branch, true) {
			return nil, nil, fmt.Errorf("failed to fetch branch %s: %v", branch, err)
		}
		log.Printf("⚠️ Fetch failed but branch exists locally, proceeding with checkout")
	}

	// Create new worktree with fun name
	funName := s.generateUniqueSessionName(repo.Path)
	// Creating worktree
	worktree, err := s.createWorktreeInternalForRepo(repo, branch, funName, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create worktree: %v", err)
	}

	// Save state
	_ = s.saveState()

	log.Printf("✅ Worktree created for existing repository: %s", repo.ID)
	return repo, worktree, nil
}

// createWorktreeInternalForRepo creates a worktree for a specific repository
func (s *GitService) createWorktreeInternalForRepo(repo *models.Repository, source, name string, isInitial bool) (*models.Worktree, error) {
	// Use WorktreeManager to create the worktree
	worktree, err := s.worktreeService.CreateWorktreeFromRequest(git.CreateWorktreeRequest{
		Repository:   repo,
		SourceBranch: source,
		BranchName:   name,
		WorkspaceDir: getWorkspaceDir(),
		IsInitial:    isInitial,
	})
	if err != nil {
		// Check if the error is because branch already exists
		if strings.Contains(err.Error(), "already exists") {
			log.Printf("⚠️  Branch %s already exists, trying a new name...", name)
			// Generate a unique name that doesn't already exist
			newName := s.generateUniqueSessionName(repo.Path)
			return s.createWorktreeInternalForRepo(repo, source, newName, isInitial)
		}
		return nil, err
	}

	// Store worktree in service map
	s.worktrees[worktree.ID] = worktree

	// Notify CommitSync service about the new worktree
	if s.commitSync != nil {
		s.commitSync.AddWorktreeWatcher(worktree.Path)
	}

	if isInitial || len(s.worktrees) == 1 {
		// Update current symlink to point to the first/initial worktree
		_ = s.updateCurrentSymlink(worktree.Path)
	}

	return worktree, nil
}

// unshallowRepository unshallows a specific branch in the background
func (s *GitService) unshallowRepository(barePath, branch string) {
	// Wait a bit before starting to avoid interfering with initial setup
	time.Sleep(5 * time.Second)

	// Only fetch the specific branch to be more efficient
	if output, err := s.runGitCommand(barePath, "fetch", "origin", "--unshallow", branch); err != nil {
		// Silent failure - unshallow is optional optimization
		_ = output // Avoid unused variable
		_ = err
	}
}

// GetRepositoryByID returns a repository by its ID
func (s *GitService) GetRepositoryByID(repoID string) *models.Repository {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.repositories[repoID]
}

// ListRepositories returns all loaded repositories
func (s *GitService) ListRepositories() []*models.Repository {
	s.mu.RLock()
	defer s.mu.RUnlock()

	repos := make([]*models.Repository, 0, len(s.repositories))
	for _, repo := range s.repositories {
		repos = append(repos, repo)
	}
	return repos
}

// GetWorktreeDiff returns the diff for a worktree against its source branch
func (s *GitService) GetWorktreeDiff(worktreeID string) (*git.WorktreeDiffResponse, error) {
	s.mu.RLock()
	worktree, exists := s.worktrees[worktreeID]
	s.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("worktree not found: %s", worktreeID)
	}

	// Get source reference and delegate to WorktreeManager
	sourceRef := s.getSourceRef(worktree)

	// Create fetch function that the WorktreeManager can call if needed
	fetchLatestRef := func(w *models.Worktree) error {
		s.fetchLatestReference(w)
		return nil
	}

	result, err := s.worktreeService.GetWorktreeDiff(worktree, sourceRef, fetchLatestRef)
	if err != nil {
		return nil, err
	}

	// Set the worktreeID since WorktreeManager doesn't have access to it
	result.WorktreeID = worktreeID
	return result, nil
}

// CreatePullRequest creates a pull request for a worktree branch
func (s *GitService) CreatePullRequest(worktreeID, title, body string) (*models.PullRequestResponse, error) {
	s.mu.RLock()
	worktree, exists := s.worktrees[worktreeID]
	if !exists {
		s.mu.RUnlock()
		return nil, fmt.Errorf("worktree %s not found", worktreeID)
	}

	repo, exists := s.repositories[worktree.RepoID]
	if !exists {
		s.mu.RUnlock()
		return nil, fmt.Errorf("repository %s not found", worktree.RepoID)
	}
	s.mu.RUnlock()

	log.Printf("🔄 Creating pull request for worktree %s", worktree.Name)

	// Check if base branch exists on remote and push if needed
	if err := s.ensureBaseBranchOnRemote(worktree, repo); err != nil {
		return nil, fmt.Errorf("failed to ensure base branch exists on remote: %v", err)
	}

	return s.githubManager.CreatePullRequest(git.CreatePullRequestRequest{
		Worktree:         worktree,
		Repository:       repo,
		Title:            title,
		Body:             body,
		IsUpdate:         false,
		FetchFullHistory: s.fetchFullHistory,
		CreateTempCommit: s.createTemporaryCommit,
		RevertTempCommit: s.revertTemporaryCommit,
	})
}

// UpdatePullRequest updates an existing pull request for a worktree branch
func (s *GitService) UpdatePullRequest(worktreeID, title, body string) (*models.PullRequestResponse, error) {
	s.mu.RLock()
	worktree, exists := s.worktrees[worktreeID]
	if !exists {
		s.mu.RUnlock()
		return nil, fmt.Errorf("worktree %s not found", worktreeID)
	}

	repo, exists := s.repositories[worktree.RepoID]
	if !exists {
		s.mu.RUnlock()
		return nil, fmt.Errorf("repository %s not found", worktree.RepoID)
	}
	s.mu.RUnlock()

	log.Printf("🔄 Updating pull request for worktree %s", worktree.Name)

	// Check if base branch exists on remote and push if needed
	if err := s.ensureBaseBranchOnRemote(worktree, repo); err != nil {
		return nil, fmt.Errorf("failed to ensure base branch exists on remote: %v", err)
	}

	return s.githubManager.CreatePullRequest(git.CreatePullRequestRequest{
		Worktree:         worktree,
		Repository:       repo,
		Title:            title,
		Body:             body,
		IsUpdate:         true,
		FetchFullHistory: s.fetchFullHistory,
		CreateTempCommit: s.createTemporaryCommit,
		RevertTempCommit: s.revertTemporaryCommit,
	})
}

// ensureBaseBranchOnRemote checks if the base branch exists on remote and pushes it if needed
func (s *GitService) ensureBaseBranchOnRemote(worktree *models.Worktree, repo *models.Repository) error {
	// For local repositories, check if base branch exists on remote
	if s.isLocalRepo(worktree.RepoID) {
		// Get the remote URL
		remoteURL, err := s.getRemoteURL(worktree.Path)
		if err != nil {
			// Try the main repo path as fallback
			remoteURL, err = s.getRemoteURL(repo.Path)
			if err != nil {
				// If no remote is configured, we can't check - assume it's handled locally
				log.Printf("⚠️ No remote configured for local repo %s, skipping base branch check", worktree.RepoID)
				return nil
			}
		}

		// Check if base branch exists on remote
		if err := s.checkBaseBranchOnRemote(worktree, remoteURL); err != nil {
			log.Printf("🔄 Base branch %s not found on remote, pushing it", worktree.SourceBranch)
			if err := s.pushBaseBranchToRemote(worktree, repo, remoteURL); err != nil {
				return fmt.Errorf("failed to push base branch to remote: %v", err)
			}
		}
	} else {
		// For remote repositories, ensure we have the latest base branch
		if err := s.fetchBaseBranchFromOrigin(worktree); err != nil {
			log.Printf("⚠️ Could not fetch base branch from origin: %v", err)
			// This is not a fatal error, continue with PR creation
		}
	}

	return nil
}

// checkBaseBranchOnRemote checks if the base branch exists on the remote repository
func (s *GitService) checkBaseBranchOnRemote(worktree *models.Worktree, remoteURL string) error {
	// Use git ls-remote to check if the base branch exists on remote
	output, err := s.runGitCommand("", "ls-remote", "--heads", remoteURL, worktree.SourceBranch)
	if err != nil {
		return fmt.Errorf("failed to check remote branches: %v", err)
	}

	// If output is empty, the branch doesn't exist on remote
	if len(strings.TrimSpace(string(output))) == 0 {
		return fmt.Errorf("base branch %s does not exist on remote", worktree.SourceBranch)
	}

	return nil
}

// pushBaseBranchToRemote pushes the base branch to the remote repository
func (s *GitService) pushBaseBranchToRemote(worktree *models.Worktree, repo *models.Repository, remoteURL string) error {
	strategy := PushStrategy{
		Branch:       worktree.SourceBranch,
		RemoteURL:    remoteURL,
		ConvertHTTPS: true,
	}

	return s.pushBranch(worktree, repo, strategy)
}

// fetchBaseBranchFromOrigin fetches the latest base branch from origin
func (s *GitService) fetchBaseBranchFromOrigin(worktree *models.Worktree) error {
	return s.fetchBranch(worktree.Path, git.FetchStrategy{
		Branch: worktree.SourceBranch,
	})
}

// syncBranchWithUpstream syncs the current branch with upstream when push fails due to being behind
func (s *GitService) syncBranchWithUpstream(worktree *models.Worktree) error {
	log.Printf("🔄 Syncing branch %s with upstream due to push failure", worktree.Branch)

	// First, fetch the latest changes from remote
	if err := s.fetchBranch(worktree.Path, git.FetchStrategy{
		Branch: worktree.Branch,
	}); err != nil {
		// If fetch fails, the branch might not exist on remote yet - that's OK
		log.Printf("⚠️ Could not fetch remote branch %s (might not exist yet): %v", worktree.Branch, err)
		return nil
	}

	// Check if we're behind the remote branch
	output, err := s.runGitCommand(worktree.Path, "rev-list", "--count", fmt.Sprintf("HEAD..origin/%s", worktree.Branch))
	if err != nil {
		// If this fails, assume we're not behind
		return nil
	}

	behindCount, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || behindCount == 0 {
		// We're not behind, no need to sync
		return nil
	}

	log.Printf("🔄 Branch %s is %d commits behind remote, syncing", worktree.Branch, behindCount)

	// Rebase our changes on top of the remote branch
	output, err = s.runGitCommand(worktree.Path, "rebase", fmt.Sprintf("origin/%s", worktree.Branch))
	if err != nil {
		// Check if this is a rebase conflict
		if strings.Contains(string(output), "CONFLICT") {
			return fmt.Errorf("rebase conflict occurred while syncing with upstream. Please resolve conflicts manually in the terminal")
		}
		return fmt.Errorf("failed to rebase on upstream: %v\n%s", err, output)
	}

	log.Printf("✅ Successfully synced branch %s with upstream", worktree.Branch)
	return nil
}

// Removed setupRemoteOrigin - remote setup is now handled by URL manager with .insteadOf

// GetPullRequestInfo gets information about an existing pull request for a worktree
func (s *GitService) GetPullRequestInfo(worktreeID string) (*models.PullRequestInfo, error) {
	s.mu.RLock()
	worktree, exists := s.worktrees[worktreeID]
	s.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("worktree %s not found", worktreeID)
	}

	// Get the repository
	repo, exists := s.repositories[worktree.RepoID]
	if !exists {
		return nil, fmt.Errorf("repository %s not found", worktree.RepoID)
	}

	// Check if branch has commits ahead of the base branch
	hasCommitsAhead, err := s.checkHasCommitsAhead(worktree)
	if err != nil {
		log.Printf("⚠️ Could not check commits ahead: %v", err)
		hasCommitsAhead = false // Default to false if we can't determine
	}

	// Initialize PR info with commits ahead status
	prInfo := &models.PullRequestInfo{
		HasCommitsAhead: hasCommitsAhead,
		Exists:          false,
	}

	// GitHubManager handles URL parsing and PR checking internally

	// Get PR info from GitHub manager (already handles checking existing PR)
	if ghPrInfo, err := s.githubManager.GetPullRequestInfo(worktree, repo); err != nil {
		log.Printf("⚠️ Could not check for existing PR: %v", err)
	} else {
		prInfo = ghPrInfo
	}

	return prInfo, nil
}

// checkHasCommitsAhead checks if the worktree branch has commits ahead of the base branch
func (s *GitService) checkHasCommitsAhead(worktree *models.Worktree) (bool, error) {
	// Ensure we have the latest base branch reference
	var baseRef string
	if s.isLocalRepo(worktree.RepoID) {
		// For local repos, use the local base branch reference
		baseRef = worktree.SourceBranch
	} else {
		// For remote repos, fetch the latest base branch and use origin reference
		if _, err := s.runGitCommand(worktree.Path, "fetch", "origin", worktree.SourceBranch); err != nil {
			log.Printf("⚠️ Could not fetch base branch %s: %v", worktree.SourceBranch, err)
		}
		baseRef = fmt.Sprintf("origin/%s", worktree.SourceBranch)
	}

	// Count commits ahead of base branch
	output, err := s.runGitCommand(worktree.Path, "rev-list", "--count", fmt.Sprintf("%s..HEAD", baseRef))
	if err != nil {
		return false, fmt.Errorf("failed to count commits ahead: %v", err)
	}

	commitCount, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return false, fmt.Errorf("failed to parse commit count: %v", err)
	}

	return commitCount > 0, nil
}
