package disk

// Google Drive archives never remove originals. Drive Desktop paths are only
// discovery hints; server identity, permissions and downloaded bytes are proof.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type DriveAccount struct {
	Account string `json:"account"`
	Mount   string `json:"desktop_mount"`
}
type DriveDiscovery struct {
	DesktopInstalled bool           `json:"desktop_installed"`
	GWSInstalled     bool           `json:"gws_installed"`
	Accounts         []DriveAccount `json:"accounts"`
	RemoteVerified   bool           `json:"remote_verified"`
}

func DiscoverMediaDrive() (DriveDiscovery, error) {
	r := DriveDiscovery{Accounts: []DriveAccount{}}
	_, err := exec.LookPath("gws")
	r.GWSInstalled = err == nil
	home, err := os.UserHomeDir()
	if err != nil {
		return r, err
	}
	for _, p := range []string{"/Applications/Google Drive.app", filepath.Join(home, "Applications/Google Drive.app")} {
		if s, err := os.Stat(p); err == nil && s.IsDir() {
			r.DesktopInstalled = true
		}
	}
	base := filepath.Join(home, "Library/CloudStorage")
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, errors.New("Drive Desktop account inventory unavailable")
	}
	for _, d := range entries {
		if strings.HasPrefix(d.Name(), "GoogleDrive-") {
			account := strings.TrimPrefix(d.Name(), "GoogleDrive-")
			if validDriveAccount.MatchString(account) && d.Type()&os.ModeSymlink == 0 {
				r.Accounts = append(r.Accounts, DriveAccount{account, filepath.Join(base, d.Name())})
			}
		}
	}
	return r, nil
}

var validDriveAccount = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]{1,100}@[A-Za-z0-9.-]{1,100}\.[A-Za-z]{2,30}$`)
var validDriveID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,200}$`)

type DriveDestination struct {
	Account       string `json:"account"`
	FolderID      string `json:"folder_id"`
	SharedDriveID string `json:"shared_drive_id,omitempty"`
	Visibility    string `json:"visibility"`
}
type DrivePermission struct {
	ID      string                  `json:"id"`
	Type    string                  `json:"type"`
	Role    string                  `json:"role"`
	Email   string                  `json:"emailAddress"`
	Domain  string                  `json:"domain"`
	Deleted bool                    `json:"deleted"`
	Details []DrivePermissionDetail `json:"permissionDetails,omitempty"`
}
type DrivePermissionDetail struct {
	Inherited bool   `json:"inherited"`
	From      string `json:"inheritedFrom,omitempty"`
	Type      string `json:"permissionType"`
	Role      string `json:"role"`
}
type DriveObject struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	MimeType   string            `json:"mimeType"`
	DriveID    string            `json:"driveId"`
	Size       string            `json:"size"`
	SHA256     string            `json:"sha256Checksum"`
	Version    string            `json:"version"`
	Trashed    bool              `json:"trashed"`
	Parents    []string          `json:"parents"`
	Properties map[string]string `json:"appProperties"`
	Owners     []struct {
		Email string `json:"emailAddress"`
	} `json:"owners"`
	Permissions  []DrivePermission `json:"permissions"`
	Capabilities struct {
		AddChildren bool `json:"canAddChildren"`
		Download    bool `json:"canDownload"`
	} `json:"capabilities"`
}
type DriveClient interface {
	Identity(context.Context) (string, error)
	Folder(context.Context, string) (DriveObject, error)
	Permissions(context.Context, string) ([]DrivePermission, error)
	ReserveIDs(context.Context, int) ([]string, error)
	Upload(context.Context, DriveDestination, MediaFile, string, string, string) error
	File(context.Context, string) (DriveObject, error)
	Download(context.Context, string, string) error
}
type GWSDrive struct {
	Account string
	binary  string
}

func NewGWSDrive(ctx context.Context, account string) (*GWSDrive, error) {
	if !validDriveAccount.MatchString(account) {
		return nil, errors.New("an explicit account email is required")
	}
	binary, err := exec.LookPath("gws")
	if err != nil {
		return nil, errors.New("gws is not installed; install/authenticate it separately, never export tokens")
	}
	d := &GWSDrive{account, binary}
	// Multi-account selection changed upstream. Qualify the installed interface,
	// then independently verify the actual server identity on every operation.
	b, err := d.run(ctx, []string{"--help"}, nil)
	if err != nil || !strings.Contains(string(b), "GOOGLE_WORKSPACE_CLI_ACCOUNT") {
		return nil, errors.New("installed gws lacks qualified explicit account selection; upgrade/configure separately")
	}
	return d, nil
}
func (d *GWSDrive) run(ctx context.Context, args []string, body any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if body != nil {
		b, _ := json.Marshal(body)
		args = append(args, "--json", string(b))
	}
	cmd := exec.CommandContext(ctx, d.binary, args...)
	for _, item := range os.Environ() {
		k, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(k, "GOOGLE_WORKSPACE_CLI_") || strings.HasSuffix(k, "API_KEY") || strings.HasSuffix(k, "API_TOKEN") || strings.HasPrefix(k, "GIT_") {
			continue
		}
		cmd.Env = append(cmd.Env, item)
	}
	cmd.Env = append(cmd.Env, "GOOGLE_WORKSPACE_CLI_ACCOUNT="+d.Account)
	out := &cappedBuffer{limit: 2 << 20}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || out.overflow {
		return nil, errors.New("gws request failed or was cancelled; no raw provider details retained")
	}
	return out.Bytes(), nil
}
func (d *GWSDrive) json(ctx context.Context, resource, method string, params any, out any) error {
	b, _ := json.Marshal(params)
	raw, err := d.run(ctx, []string{"drive", resource, method, "--params", string(b)}, nil)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, out) != nil {
		return errors.New("invalid bounded gws response")
	}
	return nil
}
func (d *GWSDrive) Identity(ctx context.Context) (string, error) {
	var r struct {
		User struct {
			Email string `json:"emailAddress"`
		} `json:"user"`
	}
	err := d.json(ctx, "about", "get", map[string]any{"fields": "user(emailAddress)"}, &r)
	if err != nil || !strings.EqualFold(r.User.Email, d.Account) {
		return "", errors.New("Drive account identity could not be verified")
	}
	return strings.ToLower(r.User.Email), nil
}

const driveObjectFields = "id,name,mimeType,driveId,size,sha256Checksum,version,trashed,parents,appProperties,owners(emailAddress),capabilities(canAddChildren,canDownload)"

func (d *GWSDrive) Permissions(ctx context.Context, id string) ([]DrivePermission, error) {
	if !validDriveID.MatchString(id) {
		return nil, errors.New("invalid permission target")
	}
	var all []DrivePermission
	token := ""
	seen := map[string]bool{}
	for page := 0; page < 10; page++ {
		params := map[string]any{"fileId": id, "supportsAllDrives": true, "pageSize": 100, "fields": "nextPageToken,permissions(id,type,role,emailAddress,domain,deleted,permissionDetails(inherited,inheritedFrom,permissionType,role))"}
		if token != "" {
			params["pageToken"] = token
		}
		var r struct {
			Permissions []DrivePermission `json:"permissions"`
			Next        string            `json:"nextPageToken"`
		}
		if err := d.json(ctx, "permissions", "list", params, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Permissions...)
		if r.Next == "" {
			return canonicalDrivePermissions(all)
		}
		if seen[r.Next] {
			return nil, errors.New("Drive permission pagination repeated")
		}
		seen[r.Next] = true
		token = r.Next
	}
	return nil, errors.New("Drive permission inventory incomplete")
}
func canonicalDrivePermissions(perms []DrivePermission) ([]DrivePermission, error) {
	if len(perms) < 1 || len(perms) > 1000 {
		return nil, errors.New("complete nonempty Drive audience required")
	}
	out := append([]DrivePermission(nil), perms...)
	seen := map[string]bool{}
	for i := range out {
		p := &out[i]
		if !validDriveID.MatchString(p.ID) || seen[p.ID] || !slices.Contains([]string{"user", "group"}, p.Type) || !validDriveAccount.MatchString(p.Email) || p.Domain != "" || p.Deleted || !slices.Contains([]string{"owner", "organizer", "fileOrganizer", "writer", "commenter", "reader"}, p.Role) {
			return nil, errors.New("Drive audience contains public/domain/unknown/deleted grants or missing principals")
		}
		seen[p.ID] = true
		p.Email = strings.ToLower(p.Email)
		p.Details = append([]DrivePermissionDetail(nil), p.Details...)
		sort.Slice(p.Details, func(i, j int) bool {
			a, _ := json.Marshal(p.Details[i])
			b, _ := json.Marshal(p.Details[j])
			return string(a) < string(b)
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func driveAudience(perms []DrivePermission) string {
	type principal struct{ Type, Email, Role string }
	a := []principal{}
	for _, p := range perms {
		a = append(a, principal{p.Type, strings.ToLower(p.Email), p.Role})
	}
	sort.Slice(a, func(i, j int) bool {
		x, _ := json.Marshal(a[i])
		y, _ := json.Marshal(a[j])
		return string(x) < string(y)
	})
	b, _ := json.Marshal(a)
	return PlanDigest(b)
}
func sharedDriveMembershipComplete(perms []DrivePermission) bool {
	for _, p := range perms {
		if len(p.Details) == 0 {
			return false
		}
		for _, d := range p.Details {
			if !slices.Contains([]string{"file", "member"}, d.Type) || !slices.Contains([]string{"organizer", "fileOrganizer", "writer", "commenter", "reader"}, d.Role) || d.Inherited && !validDriveID.MatchString(d.From) {
				return false
			}
		}
	}
	return len(perms) > 0
}

func (d *GWSDrive) File(ctx context.Context, id string) (DriveObject, error) {
	var r DriveObject
	if !validDriveID.MatchString(id) {
		return r, errors.New("invalid Drive file ID")
	}
	err := d.json(ctx, "files", "get", map[string]any{"fileId": id, "supportsAllDrives": true, "fields": driveObjectFields}, &r)
	return r, err
}
func (d *GWSDrive) Folder(ctx context.Context, id string) (DriveObject, error) {
	return d.File(ctx, id)
}
func (d *GWSDrive) ReserveIDs(ctx context.Context, n int) ([]string, error) {
	var r struct {
		IDs []string `json:"ids"`
	}
	err := d.json(ctx, "files", "generateIds", map[string]any{"count": n, "space": "drive", "type": "files"}, &r)
	if err != nil || len(r.IDs) != n {
		return nil, errors.New("remote archive IDs unavailable")
	}
	seen := map[string]bool{}
	for _, id := range r.IDs {
		if !validDriveID.MatchString(id) || seen[id] {
			return nil, errors.New("invalid remote archive IDs")
		}
		seen[id] = true
	}
	return r.IDs, nil
}
func (d *GWSDrive) Upload(ctx context.Context, dest DriveDestination, f MediaFile, archiveID, remoteID, staged string) error {
	if err := verifyStagedMedia(ctx, staged, f); err != nil {
		return err
	}
	params, _ := json.Marshal(map[string]any{"supportsAllDrives": true, "ignoreDefaultVisibility": true, "fields": "id"})
	body := map[string]any{"id": remoteID, "mimeType": "application/octet-stream", "name": filepath.Base(f.Path), "parents": []string{dest.FolderID}, "appProperties": map[string]string{"infiniteDiskArchive": archiveID, "infiniteDiskFile": f.ID, "infiniteDiskSHA256": f.SHA256}}
	b, err := d.run(ctx, []string{"drive", "files", "create", "--params", string(params), "--upload", staged}, body)
	var r struct {
		ID string `json:"id"`
	}
	if err != nil || json.Unmarshal(b, &r) != nil || r.ID != remoteID {
		return errors.New("Drive upload outcome not verified; retain originals and inspect the receipt")
	}
	return nil
}
func (d *GWSDrive) Download(ctx context.Context, id, path string) error {
	if !validDriveID.MatchString(id) {
		return errors.New("invalid remote file ID")
	}
	if noSymlinkAncestors(filepath.Dir(path)) != nil {
		return errors.New("unsafe restore directory")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("restore target must not already exist")
	}
	params, _ := json.Marshal(map[string]any{"fileId": id, "alt": "media", "supportsAllDrives": true})
	_, err := d.run(ctx, []string{"drive", "files", "get", "--params", string(params), "--output", path}, nil)
	return err
}

type DriveFolderOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (d *GWSDrive) Destinations(ctx context.Context, shared string) ([]DriveFolderOption, error) {
	if _, err := d.Identity(ctx); err != nil {
		return nil, err
	}
	if shared != "" && !validDriveID.MatchString(shared) {
		return nil, errors.New("invalid shared drive ID")
	}
	var all []DriveFolderOption
	token := ""
	for page := 0; page < 10; page++ {
		params := map[string]any{"q": "mimeType='application/vnd.google-apps.folder' and trashed=false", "pageSize": 100, "supportsAllDrives": true, "includeItemsFromAllDrives": true, "fields": "nextPageToken,incompleteSearch,files(id,name)"}
		if shared != "" {
			params["corpora"] = "drive"
			params["driveId"] = shared
		} else {
			params["corpora"] = "user"
		}
		if token != "" {
			params["pageToken"] = token
		}
		var r struct {
			Files      []DriveFolderOption `json:"files"`
			Next       string              `json:"nextPageToken"`
			Incomplete bool                `json:"incompleteSearch"`
		}
		if err := d.json(ctx, "files", "list", params, &r); err != nil {
			return nil, err
		}
		if r.Incomplete {
			return nil, errors.New("Drive destination search incomplete")
		}
		all = append(all, r.Files...)
		token = r.Next
		if token == "" {
			return all, nil
		}
	}
	return nil, errors.New("Drive destination listing exceeds page budget; choose a narrower folder ID")
}
func (d *GWSDrive) SharedDrives(ctx context.Context) ([]DriveFolderOption, error) {
	if _, err := d.Identity(ctx); err != nil {
		return nil, err
	}
	var r struct {
		Drives []DriveFolderOption `json:"drives"`
		Next   string              `json:"nextPageToken"`
	}
	if err := d.json(ctx, "drives", "list", map[string]any{"pageSize": 100, "fields": "nextPageToken,drives(id,name)"}, &r); err != nil {
		return nil, err
	}
	if r.Next != "" {
		return nil, errors.New("shared drive listing incomplete; use an explicit shared drive ID")
	}
	return r.Drives, nil
}
func validateDriveDestination(ctx context.Context, c DriveClient, d DriveDestination) (string, error) {
	proof, _, err := driveDestinationSnapshot(ctx, c, d)
	return proof, err
}
func driveDestinationSnapshot(ctx context.Context, c DriveClient, d DriveDestination) (string, []DrivePermission, error) {
	if !validDriveAccount.MatchString(d.Account) || !validDriveID.MatchString(d.FolderID) || !slices.Contains([]string{"private", "engineering-shared"}, d.Visibility) || d.Visibility == "private" && d.SharedDriveID != "" || d.Visibility == "engineering-shared" && !validDriveID.MatchString(d.SharedDriveID) {
		return "", nil, errors.New("explicit account, existing folder and private or engineering-shared destination required")
	}
	account, err := c.Identity(ctx)
	if err != nil || !strings.EqualFold(account, d.Account) {
		return "", nil, errors.New("archive account does not match the remote identity")
	}
	folder, err := c.Folder(ctx, d.FolderID)
	if err != nil || folder.ID != d.FolderID || folder.MimeType != "application/vnd.google-apps.folder" || folder.Trashed || !folder.Capabilities.AddChildren {
		return "", nil, errors.New("destination is missing, not a writable folder or permission is unknown")
	}
	folder.Permissions, err = c.Permissions(ctx, d.FolderID)
	if err != nil {
		return "", nil, errors.New("destination audience could not be completely enumerated")
	}
	folder.Permissions, err = canonicalDrivePermissions(folder.Permissions)
	if err != nil {
		return "", nil, err
	}
	if d.Visibility == "engineering-shared" {
		if folder.DriveID != d.SharedDriveID || !sharedDriveMembershipComplete(folder.Permissions) {
			return "", nil, errors.New("folder does not belong to the selected engineering shared drive")
		}
	} else {
		if folder.DriveID != "" || !privateDriveObject(folder, d.Account) {
			return "", nil, errors.New("private destination ownership or unshared visibility is not proven")
		}
	}
	// Child creation changes folder timestamps; bind only identity and access.
	b, _ := json.Marshal(struct {
		ID, DriveID  string
		Permissions  []DrivePermission
		Owners       any
		Capabilities any
	}{folder.ID, folder.DriveID, folder.Permissions, folder.Owners, folder.Capabilities})
	return PlanDigest(b), folder.Permissions, nil
}
func privateDriveObject(o DriveObject, account string) bool {
	return len(o.Owners) == 1 && strings.EqualFold(o.Owners[0].Email, account) && len(o.Permissions) == 1 && o.Permissions[0].Type == "user" && o.Permissions[0].Role == "owner" && strings.EqualFold(o.Permissions[0].Email, account)
}

type MediaArchivePlan struct {
	Version           int               `json:"version"`
	ID                string            `json:"id"`
	Mode              string            `json:"mode"`
	CreatedAt         time.Time         `json:"created_at"`
	ConfigDigest      string            `json:"config_digest"`
	SourcePlan        string            `json:"source_plan"`
	Destination       DriveDestination  `json:"destination"`
	DestinationProof  string            `json:"destination_permission_digest"`
	Audience          []DrivePermission `json:"approved_destination_audience"`
	Files             []MediaFile       `json:"files"`
	OriginalsRetained bool              `json:"originals_retained"`
}
type MediaArchiveEntry struct {
	FileID   string `json:"file_id"`
	RemoteID string `json:"remote_id"`
	Status   string `json:"status"`
	SHA256   string `json:"sha256"`
	Bytes    uint64 `json:"bytes"`
}
type MediaArchiveReceipt struct {
	Version           int                 `json:"version"`
	PlanID            string              `json:"plan_id"`
	Status            string              `json:"status"`
	ApprovedAt        time.Time           `json:"approved_at"`
	Files             []MediaArchiveEntry `json:"files"`
	OriginalsRetained bool                `json:"originals_retained"`
	DeletionApproved  bool                `json:"deletion_approved"`
	VerifiedAt        *time.Time          `json:"verified_at,omitempty"`
}

func mediaArchiveID(p MediaArchivePlan) string {
	p.ID = ""
	b, _ := json.Marshal(p)
	return PlanDigest(b)
}
func (e *Engine) PlanMediaArchive(ctx context.Context, p MediaPlan, ids []string, dest DriveDestination, c DriveClient) (MediaArchivePlan, error) {
	a := MediaArchivePlan{Version: 1, Mode: "archive-proposal", CreatedAt: e.Now().UTC(), ConfigDigest: ConfigDigest(e.Config), SourcePlan: p.ID, Destination: dest, OriginalsRetained: true}
	if _, err := e.mediaProjection(p); err != nil {
		return a, err
	}
	if err := EnsureState(e.Config); err != nil {
		return a, err
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return a, err
	}
	defer release()
	if !p.Complete || len(ids) < 1 || len(ids) > 20 {
		return a, errors.New("select 1..20 exact file IDs from a complete media report")
	}
	seen := map[string]bool{}
	var total uint64
	for _, id := range ids {
		if seen[id] {
			return a, errors.New("duplicate file selection")
		}
		seen[id] = true
		index := slices.IndexFunc(p.Files, func(f MediaFile) bool { return f.ID == id })
		if index < 0 {
			return a, errors.New("file ID not in media report")
		}
		f := p.Files[index]
		if err := e.VerifyMediaFile(ctx, f); err != nil {
			return a, err
		}
		if f.Bytes > 128<<20 || total+f.Bytes > 512<<20 {
			return a, errors.New("archive batch exceeds 128 MiB/file or 512 MiB/batch; narrow the selection")
		}
		sum, err := hashMedia(ctx, f.Path, f.Identity, f.Bytes)
		if err != nil {
			return a, err
		}
		f.SHA256 = sum
		total += f.Bytes
		a.Files = append(a.Files, f)
	}
	proof, audience, err := driveDestinationSnapshot(ctx, c, dest)
	if err != nil {
		return a, err
	}
	a.DestinationProof = proof
	a.Audience = audience
	a.ID = mediaArchiveID(a)
	b, _ := json.MarshalIndent(a, "", "  ")
	err = atomicPrivate(filepath.Join(e.Config.StateDir, "media-archives", a.ID, "plan.json"), b)
	return a, err
}
func (e *Engine) LoadMediaArchive(id string) (MediaArchivePlan, error) {
	var p MediaArchivePlan
	if !validPlanID.MatchString(id) {
		return p, errors.New("exact archive plan ID required")
	}
	path := filepath.Join(e.Config.StateDir, "media-archives", id, "plan.json")
	if noSymlinkAncestors(path) != nil {
		return p, errors.New("unsafe archive plan")
	}
	s, err := os.Lstat(path)
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() > 2<<20 {
		return p, errors.New("archive plan unavailable")
	}
	b, err := readMediaPrivate(path, 2<<20)
	if err != nil || json.Unmarshal(b, &p) != nil || p.ID != id || mediaArchiveID(p) != id || p.ConfigDigest != ConfigDigest(e.Config) || p.Mode != "archive-proposal" || !p.OriginalsRetained {
		return p, errors.New("archive plan changed")
	}
	return p, e.validateMediaArchive(p)
}
func (e *Engine) validateMediaArchive(p MediaArchivePlan) error {
	if _, err := canonicalDrivePermissions(p.Audience); err != nil {
		return err
	}
	if !validPlanID.MatchString(p.ID) || p.ID != mediaArchiveID(p) || p.Version != 1 || p.ConfigDigest != ConfigDigest(e.Config) || p.Mode != "archive-proposal" || !p.OriginalsRetained || !validPlanID.MatchString(p.DestinationProof) || len(p.Files) < 1 || len(p.Files) > 20 {
		return errors.New("invalid archive plan")
	}
	source, err := e.LoadMedia(p.SourcePlan)
	if err != nil {
		return err
	}
	if _, err = e.mediaProjection(source); err != nil || !source.Complete {
		return errors.New("source media report incomplete or changed")
	}
	seen := map[string]bool{}
	var total uint64
	for _, f := range p.Files {
		index := slices.IndexFunc(source.Files, func(s MediaFile) bool { return s.ID == f.ID })
		if index < 0 || seen[f.ID] || !validPlanID.MatchString(f.SHA256) || f.Bytes > 128<<20 {
			return errors.New("invalid archive source selection")
		}
		seen[f.ID] = true
		original := source.Files[index]
		original.SHA256 = f.SHA256
		a, _ := json.Marshal(original)
		b, _ := json.Marshal(f)
		if string(a) != string(b) {
			return errors.New("archive file differs from source report")
		}
		total += f.Bytes
	}
	if total > 512<<20 {
		return errors.New("archive batch over budget")
	}
	return nil
}
func (e *Engine) saveMediaArchiveReceipt(r MediaArchiveReceipt) error {
	b, _ := json.MarshalIndent(r, "", "  ")
	return atomicPrivate(filepath.Join(e.Config.StateDir, "media-archives", r.PlanID, "receipt.json"), b)
}
func (e *Engine) UploadMediaArchive(ctx context.Context, p MediaArchivePlan, approval string, ackShared bool, c DriveClient) (r MediaArchiveReceipt, err error) {
	if err = e.validateMediaArchive(p); err != nil {
		return r, err
	}
	if approval != p.ID || p.ID != mediaArchiveID(p) || p.ConfigDigest != ConfigDigest(e.Config) || p.Mode != "archive-proposal" || !p.OriginalsRetained || e.Now().Sub(p.CreatedAt) > 24*time.Hour || p.CreatedAt.After(e.Now().Add(time.Minute)) || len(p.Files) < 1 || len(p.Files) > 20 {
		return r, errors.New("exact current archive approval required")
	}
	if p.Destination.Visibility == "engineering-shared" && !ackShared {
		return r, errors.New("engineering sharing needs --acknowledge-shared; personal screenshots are never shared by default")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return r, err
	}
	defer release()
	receiptPath := filepath.Join(e.Config.StateDir, "media-archives", p.ID, "receipt.json")
	if _, err = os.Lstat(receiptPath); !errors.Is(err, os.ErrNotExist) {
		return r, errors.New("archive already attempted; verify its receipt instead of reuploading")
	}
	proof, err := validateDriveDestination(ctx, c, p.Destination)
	if err != nil || proof != p.DestinationProof {
		return r, errors.New("archive destination/account/permissions changed")
	}
	for _, f := range p.Files {
		if err = e.VerifyMediaFile(ctx, f); err != nil {
			return r, err
		}
	}
	ids, err := c.ReserveIDs(ctx, len(p.Files))
	if err != nil || len(ids) != len(p.Files) {
		return r, errors.New("archive upload IDs unavailable")
	}
	r = MediaArchiveReceipt{Version: 1, PlanID: p.ID, Status: "in_progress", ApprovedAt: e.Now().UTC(), OriginalsRetained: true}
	seen := map[string]bool{}
	for i, f := range p.Files {
		if !validDriveID.MatchString(ids[i]) || seen[ids[i]] {
			return MediaArchiveReceipt{}, errors.New("invalid generated remote IDs")
		}
		seen[ids[i]] = true
		r.Files = append(r.Files, MediaArchiveEntry{FileID: f.ID, RemoteID: ids[i], Status: "reserved", SHA256: f.SHA256, Bytes: f.Bytes})
	}
	if err = e.saveMediaArchiveReceipt(r); err != nil {
		return r, err
	}
	defer func() {
		if err != nil {
			r.Status = "incomplete_originals_retained"
			if saveErr := e.saveMediaArchiveReceipt(r); saveErr != nil {
				err = errors.New("archive incomplete and receipt could not be updated; originals retained")
			}
		}
	}()
	for i, f := range p.Files {
		if err = e.VerifyMediaFile(ctx, f); err != nil {
			return r, err
		}
		proof, err = validateDriveDestination(ctx, c, p.Destination)
		if err != nil || proof != p.DestinationProof {
			return r, errors.New("archive destination changed before upload")
		}
		scratch, makeErr := os.MkdirTemp(e.Config.StateDir, "media-transfer-")
		if makeErr != nil {
			return r, makeErr
		}
		err = func() error {
			defer os.RemoveAll(scratch)
			staged := filepath.Join(scratch, "upload")
			if err := stageMediaCopy(ctx, f, staged); err != nil {
				return err
			}
			if err := verifyStagedMedia(ctx, staged, f); err != nil {
				return err
			}
			if err := c.Upload(ctx, p.Destination, f, p.ID, ids[i], staged); err != nil {
				return err
			}
			r.Files[i].Status = "uploaded_unverified"
			if err := e.saveMediaArchiveReceipt(r); err != nil {
				return err
			}
			return verifyDriveCopy(ctx, c, p, f, ids[i], filepath.Join(scratch, "restored"))
		}()
		if err != nil {
			return r, err
		}
		r.Files[i].Status = "verified_download_sha256"
		if err = e.saveMediaArchiveReceipt(r); err != nil {
			return r, err
		}
	}
	for _, f := range p.Files {
		if err = e.VerifyMediaFile(ctx, f); err != nil {
			return r, err
		}
	}
	r.Status = "verified_archive_originals_retained"
	now := e.Now().UTC()
	r.VerifiedAt = &now
	err = e.saveMediaArchiveReceipt(r)
	return r, err
}
func stageMediaCopy(ctx context.Context, f MediaFile, dest string) error {
	if noSymlinkAncestors(filepath.Dir(dest)) != nil {
		return errors.New("unsafe staging directory")
	}
	fd, err := syscallOpenMedia(f.Path)
	if err != nil {
		return err
	}
	defer fd.Close()
	before, err := fd.Stat()
	if err != nil {
		return err
	}
	id, _, err := mediaStat(before)
	if err != nil || id != f.Identity || mediaOffline(before) {
		return errors.New("source changed before staging")
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, io.LimitReader(&privateContextReader{ctx, fd}, int64(f.Bytes)+1))
	syncErr := out.Sync()
	closeErr := out.Close()
	after, statErr := fd.Stat()
	if copyErr != nil || syncErr != nil || closeErr != nil || statErr != nil {
		return errors.New("archive staging incomplete")
	}
	afterID, _, err := mediaStat(after)
	if err != nil || afterID != f.Identity {
		return errors.New("source changed during staging")
	}
	sum, err := digestFile(dest)
	s, statErr := os.Stat(dest)
	if err != nil || statErr != nil || sum != f.SHA256 || uint64(s.Size()) != f.Bytes {
		return errors.New("archive staging hash mismatch")
	}
	if noSymlinkAncestors(dest) != nil {
		return errors.New("staging path replaced")
	}
	return os.Chmod(dest, 0400)
}
func verifyStagedMedia(ctx context.Context, path string, f MediaFile) error {
	if noSymlinkAncestors(path) != nil {
		return errors.New("unsafe staging path")
	}
	s, err := os.Lstat(path)
	if err != nil {
		return errors.New("staged file unavailable")
	}
	id, _, err := mediaStat(s)
	if err != nil || uint64(s.Size()) != f.Bytes || s.Mode().Perm()&0222 != 0 {
		return errors.New("staged file changed")
	}
	sum, err := hashMedia(ctx, path, id, f.Bytes)
	if err != nil || sum != f.SHA256 {
		return errors.New("staged file hash changed")
	}
	return nil
}
func verifyDriveCopy(ctx context.Context, c DriveClient, p MediaArchivePlan, f MediaFile, id, path string) error {
	proof, err := validateDriveDestination(ctx, c, p.Destination)
	if err != nil || proof != p.DestinationProof {
		return errors.New("archive destination changed during verification")
	}
	before, err := c.File(ctx, id)
	if err == nil {
		before.Permissions, err = c.Permissions(ctx, id)
	}
	valid := func(o DriveObject) bool {
		size, sizeErr := strconv.ParseUint(o.Size, 10, 64)
		perms, aclErr := canonicalDrivePermissions(o.Permissions)
		private := aclErr == nil && driveAudience(perms) == driveAudience(p.Audience) && (p.Destination.Visibility != "private" || privateDriveObject(o, p.Destination.Account)) && (p.Destination.Visibility != "engineering-shared" || sharedDriveMembershipComplete(perms))
		return private && o.ID == id && !o.Trashed && o.Name == filepath.Base(f.Path) && o.MimeType == "application/octet-stream" && o.Version != "" && sizeErr == nil && size == f.Bytes && o.Capabilities.Download && len(o.Parents) == 1 && o.Parents[0] == p.Destination.FolderID && o.DriveID == p.Destination.SharedDriveID && o.Properties["infiniteDiskArchive"] == p.ID && o.Properties["infiniteDiskFile"] == f.ID && o.Properties["infiniteDiskSHA256"] == f.SHA256 && (o.SHA256 == "" || o.SHA256 == f.SHA256)
	}
	if err != nil || !valid(before) {
		return errors.New("remote archive metadata does not match the exact plan")
	}
	if err = c.Download(ctx, id, path); err != nil {
		return err
	}
	s, err := os.Lstat(path)
	if err != nil || !s.Mode().IsRegular() || uint64(s.Size()) != f.Bytes || noSymlinkAncestors(path) != nil {
		return errors.New("remote restore missing or truncated")
	}
	identity, _, err := mediaStat(s)
	if err != nil {
		return errors.New("unsafe restore file")
	}
	sum, err := hashMedia(ctx, path, identity, f.Bytes)
	if err != nil || sum != f.SHA256 {
		return errors.New("remote restore SHA256 mismatch")
	}
	after, err := c.File(ctx, id)
	if err == nil {
		after.Permissions, err = c.Permissions(ctx, id)
	}
	if err != nil || !valid(after) || after.Version != before.Version {
		return errors.New("remote archive changed during restore verification")
	}
	return nil
}
func (e *Engine) VerifyMediaArchive(ctx context.Context, p MediaArchivePlan, c DriveClient) (r MediaArchiveReceipt, err error) {
	if e.validateMediaArchive(p) != nil {
		return r, errors.New("invalid archive plan")
	}
	path := filepath.Join(e.Config.StateDir, "media-archives", p.ID, "receipt.json")
	if noSymlinkAncestors(path) != nil {
		return r, errors.New("unsafe archive receipt")
	}
	s, statErr := os.Lstat(path)
	if statErr != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() > 2<<20 {
		return r, errors.New("unsafe archive receipt")
	}
	b, err := readMediaPrivate(path, 2<<20)
	if err != nil || len(b) > 2<<20 || json.Unmarshal(b, &r) != nil || r.PlanID != p.ID || len(r.Files) != len(p.Files) || !r.OriginalsRetained || r.DeletionApproved {
		return r, errors.New("archive receipt unavailable or invalid")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return r, err
	}
	defer release()
	defer func() {
		if err != nil {
			r.Status = "verification_failed_originals_retained"
			r.VerifiedAt = nil
			if e.saveMediaArchiveReceipt(r) != nil {
				err = errors.New("verification failed and receipt update failed; originals retained")
			}
		}
	}()
	seen := map[string]bool{}
	for i, f := range p.Files {
		if r.Files[i].FileID != f.ID || r.Files[i].SHA256 != f.SHA256 || r.Files[i].Bytes != f.Bytes || !validDriveID.MatchString(r.Files[i].RemoteID) || seen[r.Files[i].RemoteID] {
			return r, errors.New("archive receipt identity changed")
		}
		seen[r.Files[i].RemoteID] = true
		if err = e.VerifyMediaFile(ctx, f); err != nil {
			return r, err
		}
		scratch, err := os.MkdirTemp(e.Config.StateDir, "media-restore-")
		if err != nil {
			return r, err
		}
		err = verifyDriveCopy(ctx, c, p, f, r.Files[i].RemoteID, filepath.Join(scratch, "restored"))
		os.RemoveAll(scratch)
		if err != nil {
			return r, err
		}
		r.Files[i].Status = "verified_download_sha256"
	}
	r.Status = "verified_archive_originals_retained"
	now := e.Now().UTC()
	r.VerifiedAt = &now
	err = e.saveMediaArchiveReceipt(r)
	return r, err
}
func syscallOpenMedia(path string) (*os.File, error) {
	if noSymlinkAncestors(path) != nil {
		return nil, errors.New("source path changed")
	}
	return openMediaNoFollow(path)
}
func (d DriveDestination) String() string {
	return fmt.Sprintf("%s / %s / %s", d.Account, d.Visibility, d.FolderID)
}
