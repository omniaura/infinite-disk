package disk

import "time"

const ProtocolVersion = 1

type Config struct {
	Version            int      `json:"version"`
	Roots              []string `json:"roots"`
	StateDir           string   `json:"state_dir"`
	IdleDays           int      `json:"idle_days"`
	MinFreeGiB         uint64   `json:"min_free_gib"`
	MinFreePercent     float64  `json:"min_free_percent"`
	PollSeconds        int      `json:"poll_seconds"`
	AuditMinutes       int      `json:"audit_minutes"`
	PeriodicHours      int      `json:"periodic_hours"`
	MaxProjects        int      `json:"max_projects"`
	MaxDepth           int      `json:"max_depth"`
	AgentEnabled       bool     `json:"agent_enabled"`
	AgentModel         string   `json:"agent_model"`
	AgentProvider      string   `json:"agent_provider,omitempty"`
	AgentBaseURL       string   `json:"agent_base_url,omitempty"`
	AgentKeyFile       string   `json:"agent_key_file,omitempty"`
	AllowedRemoteHosts []string `json:"allowed_remote_hosts"`
}

type Evidence struct {
	Check   string `json:"check"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

type Candidate struct {
	ID              string           `json:"id"`
	Path            string           `json:"path"`
	Name            string           `json:"name"`
	Kind            string           `json:"kind"`
	Head            string           `json:"head,omitempty"`
	Branch          string           `json:"branch,omitempty"`
	CommonDir       string           `json:"common_dir,omitempty"`
	RemoteHost      string           `json:"remote_host,omitempty"`
	RemoteDigest    string           `json:"remote_digest,omitempty"`
	SizeBytes       uint64           `json:"size_bytes"`
	SizeComplete    bool             `json:"size_complete"`
	LastModified    time.Time        `json:"last_modified"`
	DirtyFiles      int              `json:"dirty_files"`
	Fingerprint     string           `json:"fingerprint,omitempty"`
	Eligible        bool             `json:"eligible"`
	Blockers        []string         `json:"blockers"`
	Evidence        []Evidence       `json:"evidence"`
	Sessions        []Session        `json:"sessions,omitempty"`
	SessionCount    int              `json:"session_count,omitempty"`
	SessionsChecked bool             `json:"sessions_checked,omitempty"`
	SessionHandoffs []ClaudeHandoff  `json:"session_handoffs,omitempty"`
	Analysis        *ProjectAnalysis `json:"analysis,omitempty"`
}

type Space struct {
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	FreePercent float64 `json:"free_percent"`
}

type Plan struct {
	Version            int              `json:"version"`
	ID                 string           `json:"id"`
	CreatedAt          time.Time        `json:"created_at"`
	DiscoveredAt       *time.Time       `json:"discovered_at,omitempty"`
	DiscoveryCached    bool             `json:"discovery_cached,omitempty"`
	Mode               string           `json:"mode"`
	ConfigDigest       string           `json:"config_digest"`
	Space              Space            `json:"space"`
	Candidates         []Candidate      `json:"candidates"`
	Warnings           []string         `json:"warnings"`
	Recommendations    []Recommendation `json:"recommendations,omitempty"`
	AgentSteps         []Evidence       `json:"agent_steps,omitempty"`
	Locations          []Location       `json:"locations,omitempty"`
	DiscoveredProjects int              `json:"discovered_projects,omitempty"`
	DiscoveryComplete  bool             `json:"discovery_complete,omitempty"`
	SessionWarnings    []string         `json:"session_warnings,omitempty"`
}

type Recommendation struct {
	CandidateID      string   `json:"candidate_id"`
	Priority         int      `json:"priority"`
	Decision         string   `json:"decision"`
	Reason           string   `json:"reason"`
	ProposedSteps    []string `json:"proposed_steps"`
	SuggestedActions []string `json:"suggested_actions,omitempty"`
}

type Receipt struct {
	Version         int             `json:"version"`
	PlanID          string          `json:"plan_id"`
	CandidateID     string          `json:"candidate_id"`
	Path            string          `json:"path"`
	ApprovedAt      time.Time       `json:"approved_at"`
	Result          string          `json:"result"`
	RecoveryRef     string          `json:"recovery_ref,omitempty"`
	RecoveryCommit  string          `json:"recovery_commit,omitempty"`
	RemoteURL       string          `json:"remote_url,omitempty"`
	QuarantinePath  string          `json:"quarantine_path,omitempty"`
	BundlePath      string          `json:"bundle_path,omitempty"`
	BundleSHA256    string          `json:"bundle_sha256,omitempty"`
	BeforeSpace     Space           `json:"before_space"`
	AfterSpace      Space           `json:"after_space"`
	Steps           []Evidence      `json:"steps"`
	SessionHandoffs []ClaudeHandoff `json:"session_handoffs,omitempty"`
}
