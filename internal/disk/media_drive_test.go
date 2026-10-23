package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

type fixtureDrive struct {
	account                string
	folder                 DriveObject
	objects                map[string]DriveObject
	data                   map[string][]byte
	uploads                int
	uploadErr, downloadErr bool
	corrupt, changed       bool
	changePermissions      bool
	hook                   func(string)
}

func newFixtureDrive() *fixtureDrive {
	d := &fixtureDrive{account: "fixture@example.invalid", objects: map[string]DriveObject{}, data: map[string][]byte{}}
	d.folder = DriveObject{ID: "folder_123456", MimeType: "application/vnd.google-apps.folder", Permissions: []DrivePermission{{ID: "permission_1234", Type: "user", Role: "owner", Email: d.account}}}
	d.folder.Owners = append(d.folder.Owners, struct {
		Email string `json:"emailAddress"`
	}{d.account})
	d.folder.Capabilities.AddChildren = true
	return d
}
func (d *fixtureDrive) Identity(context.Context) (string, error)            { return d.account, nil }
func (d *fixtureDrive) Folder(context.Context, string) (DriveObject, error) { return d.folder, nil }
func (d *fixtureDrive) Permissions(_ context.Context, id string) ([]DrivePermission, error) {
	if id == d.folder.ID {
		return d.folder.Permissions, nil
	}
	o, ok := d.objects[id]
	if !ok {
		return nil, errors.New("missing permissions")
	}
	return o.Permissions, nil
}
func (d *fixtureDrive) ReserveIDs(_ context.Context, n int) ([]string, error) {
	ids := []string{}
	for i := 0; i < n; i++ {
		ids = append(ids, fmt.Sprintf("remote_1234_%d", i))
	}
	return ids, nil
}
func (d *fixtureDrive) Upload(ctx context.Context, dest DriveDestination, f MediaFile, plan, id, path string) error {
	if d.hook != nil {
		d.hook(path)
	}
	if err := verifyStagedMedia(ctx, path, f); err != nil {
		return err
	}
	d.uploads++
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	o := DriveObject{ID: id, Name: filepath.Base(f.Path), MimeType: "application/octet-stream", Size: strconv.Itoa(len(b)), Version: "1", DriveID: dest.SharedDriveID, Parents: []string{dest.FolderID}, Properties: map[string]string{"infiniteDiskArchive": plan, "infiniteDiskFile": f.ID, "infiniteDiskSHA256": f.SHA256}, SHA256: f.SHA256, Owners: d.folder.Owners, Permissions: d.folder.Permissions}
	o.Capabilities.Download = true
	d.objects[id] = o
	d.data[id] = b
	if d.uploadErr {
		return errors.New("interrupted fixture upload")
	}
	return nil
}
func (d *fixtureDrive) File(_ context.Context, id string) (DriveObject, error) {
	o, ok := d.objects[id]
	if !ok {
		return o, errors.New("missing fixture")
	}
	if d.changed {
		o.Version = "2"
	}
	return o, nil
}
func (d *fixtureDrive) Download(_ context.Context, id, path string) error {
	if d.downloadErr {
		return errors.New("fixture offline")
	}
	b := d.data[id]
	if d.corrupt {
		b = []byte("corrupt")
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		return err
	}
	if d.changePermissions {
		o := d.objects[id]
		o.Permissions = append(o.Permissions, DrivePermission{ID: "public_123456", Type: "anyone", Role: "reader"})
		d.objects[id] = o
	}
	if d.changed {
		o := d.objects[id]
		o.Version = "3"
		d.objects[id] = o
		d.changed = false
	}
	return nil
}
func archiveFixture(t *testing.T) (*Engine, MediaArchivePlan, *fixtureDrive) {
	t.Helper()
	e, root := mediaFixture(t)
	mediaWrite(t, root, "Screenshot \x1b[31m UNTRUSTED.png", "archive me")
	p := mediaScan(t, e, root)
	d := newFixtureDrive()
	a, err := e.PlanMediaArchive(context.Background(), p, []string{p.Files[0].ID}, DriveDestination{Account: d.account, FolderID: d.folder.ID, Visibility: "private"}, d)
	if err != nil {
		t.Fatal(err)
	}
	return e, a, d
}
func TestMediaDriveArchiveApprovalReadbackAndOriginals(t *testing.T) {
	e, p, d := archiveFixture(t)
	if d.uploads != 0 {
		t.Fatal("planning uploaded")
	}
	if _, err := e.UploadMediaArchive(context.Background(), p, "wrong", false, d); err == nil {
		t.Fatal("approval bypass")
	}
	loaded, err := e.LoadMediaArchive(p.ID)
	if err != nil || loaded.ID != p.ID {
		t.Fatal(err)
	}
	r, err := e.UploadMediaArchive(context.Background(), p, p.ID, false, d)
	if err != nil || r.Status != "verified_archive_originals_retained" || !r.OriginalsRetained || r.DeletionApproved || d.uploads != 1 {
		t.Fatal(r, err)
	}
	for _, f := range p.Files {
		exists(t, f.Path)
	}
	if _, err = e.UploadMediaArchive(context.Background(), p, p.ID, false, d); err == nil {
		t.Fatal("duplicate reupload accepted")
	}
	if _, err = e.VerifyMediaArchive(context.Background(), p, d); err != nil {
		t.Fatal(err)
	}
	d.corrupt = true
	if _, err = e.VerifyMediaArchive(context.Background(), p, d); err == nil {
		t.Fatal("corrupt cloud copy accepted")
	}
	for _, f := range p.Files {
		exists(t, f.Path)
	}
}
func TestMediaDriveDestinationIdentitySharingAndSourceChanges(t *testing.T) {
	for _, kind := range []string{"wrong_account", "shared_as_private", "no_permission", "changed_source", "staging_symlink", "interrupted", "download_failure", "remote_missing", "remote_shared", "remote_version", "receipt_symlink"} {
		t.Run(kind, func(t *testing.T) {
			e, p, d := archiveFixture(t)
			switch kind {
			case "wrong_account":
				d.account = "other@example.invalid"
			case "shared_as_private":
				d.folder.DriveID = "shared_12345"
			case "no_permission":
				d.folder.Capabilities.AddChildren = false
			case "changed_source":
				write(t, p.Files[0].Path, "replaced")
			case "staging_symlink":
				d.hook = func(path string) {
					os.Remove(path)
					if err := os.Symlink(p.Files[0].Path, path); err != nil {
						t.Fatal(err)
					}
				}
			case "interrupted":
				d.uploadErr = true
			case "download_failure":
				d.downloadErr = true
			}
			r, err := e.UploadMediaArchive(context.Background(), p, p.ID, false, d)
			if kind == "remote_missing" || kind == "remote_shared" || kind == "remote_version" || kind == "receipt_symlink" {
				if err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "remote_missing":
					clear(d.objects)
				case "remote_shared":
					for id, o := range d.objects {
						o.Permissions = append(o.Permissions, DrivePermission{Type: "anyone", Role: "reader"})
						d.objects[id] = o
					}
				case "remote_version":
					d.changed = true
				case "receipt_symlink":
					path := filepath.Join(e.Config.StateDir, "media-archives", p.ID, "receipt.json")
					os.Remove(path)
					if err = os.Symlink(p.Files[0].Path, path); err != nil {
						t.Fatal(err)
					}
				}
				_, err = e.VerifyMediaArchive(context.Background(), p, d)
			}
			if err == nil {
				t.Fatal("unsafe state accepted", kind)
			}
			if kind == "interrupted" && r.Status != "incomplete_originals_retained" {
				t.Fatal(r)
			}
			exists(t, p.Files[0].Path)
		})
	}
}
func TestMediaDriveSharedExplicitAcknowledgement(t *testing.T) {
	e, root := mediaFixture(t)
	mediaWrite(t, root, "engineering.png", "fixture")
	source := mediaScan(t, e, root)
	d := newFixtureDrive()
	d.folder.DriveID = "shared_123456"
	d.folder.Owners = nil
	d.folder.Permissions = []DrivePermission{{ID: "engineering_group123", Type: "group", Role: "writer", Email: "engineering@example.invalid", Details: []DrivePermissionDetail{{Inherited: true, From: d.folder.DriveID, Type: "member", Role: "writer"}}}}
	dest := DriveDestination{Account: d.account, FolderID: d.folder.ID, Visibility: "engineering-shared", SharedDriveID: d.folder.DriveID}
	p, err := e.PlanMediaArchive(context.Background(), source, []string{source.Files[0].ID}, dest, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.UploadMediaArchive(context.Background(), p, p.ID, false, d); err == nil {
		t.Fatal("sharing unacknowledged")
	}
	if _, err = e.UploadMediaArchive(context.Background(), p, p.ID, true, d); err != nil {
		t.Fatal(err)
	}
}
func TestMediaDriveCancelledAndMalformedPlans(t *testing.T) {
	e, p, d := archiveFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.UploadMediaArchive(ctx, p, p.ID, false, d); err == nil {
		t.Fatal("cancel ignored")
	}
	if d.uploads != 0 {
		t.Fatal("cancelled operation uploaded")
	}
	p.Files[0].Bytes = 1 << 40
	p.ID = mediaArchiveID(p)
	if _, err := e.UploadMediaArchive(context.Background(), p, p.ID, false, d); err == nil {
		t.Fatal("over-budget forged plan accepted")
	}
}
func TestMediaSharedAudienceCompleteStableAndChangesFail(t *testing.T) {
	e, root := mediaFixture(t)
	mediaWrite(t, root, "engineering.png", "fixture")
	source := mediaScan(t, e, root)
	d := newFixtureDrive()
	d.folder.DriveID = "shared_123456"
	d.folder.Owners = nil
	group := DrivePermission{ID: "engineering_group123", Type: "group", Role: "writer", Email: "engineering@example.invalid", Details: []DrivePermissionDetail{{Inherited: true, From: d.folder.DriveID, Type: "member", Role: "writer"}}}
	user := DrivePermission{ID: "engineering_user123", Type: "user", Role: "reader", Email: "fixture@example.invalid", Details: []DrivePermissionDetail{{Inherited: true, From: d.folder.DriveID, Type: "member", Role: "reader"}}}
	dest := DriveDestination{Account: d.account, FolderID: d.folder.ID, Visibility: "engineering-shared", SharedDriveID: d.folder.DriveID}
	for _, perms := range [][]DrivePermission{nil, {{ID: "public_123456", Type: "anyone", Role: "reader"}}, {{ID: "domain_123456", Type: "domain", Domain: "example.invalid", Role: "reader"}}, {{ID: "missing_details123", Type: "group", Email: group.Email, Role: "writer"}}} {
		d.folder.Permissions = perms
		if _, err := e.PlanMediaArchive(context.Background(), source, []string{source.Files[0].ID}, dest, d); err == nil {
			t.Fatal("unproven shared audience accepted")
		}
	}
	d.folder.Permissions = []DrivePermission{group, user}
	p, err := e.PlanMediaArchive(context.Background(), source, []string{source.Files[0].ID}, dest, d)
	if err != nil {
		t.Fatal(err)
	}
	d.folder.Permissions = []DrivePermission{user, group}
	r, err := e.UploadMediaArchive(context.Background(), p, p.ID, true, d)
	if err != nil {
		t.Fatal("order-only ACL change blocked", err)
	}
	o := d.objects[r.Files[0].RemoteID]
	o.Permissions = []DrivePermission{group, user}
	d.objects[o.ID] = o
	if _, err = e.VerifyMediaArchive(context.Background(), p, d); err != nil {
		t.Fatal("order-only remote ACL change blocked", err)
	}
	d.changePermissions = true
	if _, err = e.VerifyMediaArchive(context.Background(), p, d); err == nil {
		t.Fatal("ACL changed during restore accepted")
	}
	d.changePermissions = false
	o.Permissions = append(o.Permissions, DrivePermission{ID: "public_123456", Type: "anyone", Role: "reader"})
	d.objects[o.ID] = o
	r, err = e.VerifyMediaArchive(context.Background(), p, d)
	if err == nil || r.Status != "verification_failed_originals_retained" || r.VerifiedAt != nil {
		t.Fatal("public ACL accepted", r, err)
	}
	exists(t, p.Files[0].Path)
}
