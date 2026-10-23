package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
)

func mediaCommand(load func() (*disk.Engine, error), asJSON *bool) *cobra.Command {
	root := &cobra.Command{Use: "media", Short: "Review local screenshots and Downloads; never delete originals"}
	emit := func(cmd *cobra.Command, v any) error { return json.NewEncoder(cmd.OutOrStdout()).Encode(v) }
	var roots []string
	var files, depth int
	var hashMiB uint64
	var agent bool
	scan := &cobra.Command{Use: "scan", Short: "Inventory explicit user-media folders and exact duplicates (report only)", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		if hashMiB < 1 || hashMiB > 16384 || files < 1 || files > 20000 || depth < 1 || depth > 12 {
			return errors.New("invalid media limits")
		}
		if !*asJSON {
			fmt.Fprintln(cmd.ErrOrStderr(), "Scanning local metadata; hashing size-colliding files within the budget. No image upload, OCR, move or deletion. Ctrl+C cancels.")
		}
		p, err := e.MediaScan(cmd.Context(), disk.MediaOptions{Roots: roots, MaxFiles: files, MaxDepth: depth, HashBudget: hashMiB << 20})
		if err != nil {
			return err
		}
		if *asJSON {
			if err = emit(cmd, p); err != nil {
				return err
			}
		} else {
			renderMedia(cmd.OutOrStdout(), p, false)
		}
		if agent {
			a, err := e.TriageMedia(cmd.Context(), p)
			if err != nil {
				return err
			}
			if *asJSON {
				return emit(cmd, a)
			}
			renderMediaAdvice(cmd.OutOrStdout(), a)
		}
		return nil
	}}
	scan.Flags().StringSliceVar(&roots, "root", nil, "Explicit existing folder inside Downloads, Desktop or Pictures (repeatable); defaults Downloads/Desktop/Pictures/Screenshots")
	scan.Flags().IntVar(&files, "limit", 5000, "Maximum inventoried files (1..20000); partial inventories hold every file")
	scan.Flags().IntVar(&depth, "depth", 6, "Maximum directory depth (1..12)")
	scan.Flags().Uint64Var(&hashMiB, "hash-mib", 2048, "Hash budget in MiB (at most 16384); files over 128 MiB stay metadata-only")
	scan.Flags().BoolVar(&agent, "agent", false, "Explicit bounded metadata-only Pi triage using configured Ditto/OpenRouter; never sends images or names")
	var source string
	var details bool
	review := &cobra.Command{Use: "review", Short: "Show largest files, exact duplicates and reasons to keep files", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := e.LoadMedia(source)
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(cmd, p)
		}
		renderMedia(cmd.OutOrStdout(), p, details)
		return nil
	}}
	review.Flags().StringVar(&source, "plan", "latest", "Exact immutable media report ID")
	review.Flags().BoolVar(&details, "details", false, "Show every file ID, local path and hold reasons")
	var triagePlan string
	triage := &cobra.Command{Use: "triage", Short: "Ask Pi for metadata-only media review suggestions; apply nothing", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := e.LoadMedia(triagePlan)
		if err != nil {
			return err
		}
		a, err := e.TriageMedia(cmd.Context(), p)
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(cmd, a)
		}
		renderMediaAdvice(cmd.OutOrStdout(), a)
		return nil
	}}
	triage.Flags().StringVar(&triagePlan, "plan", "latest", "Exact media report ID; at most 20 largest records sent")
	var decisionPlan, key string
	decide := &cobra.Command{Use: "decide", Short: "One Jev/OpenRouter typed metadata request; no pixels or actions", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := e.LoadMedia(decisionPlan)
		if err != nil {
			return err
		}
		r, err := e.DecideMedia(cmd.Context(), p, key)
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(cmd, r)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Jev suggestions only: %d files, %d request, %s. No approval or execution.\n", len(r.Answers), r.Requests, plain(r.ServedModel))
		for _, a := range r.Answers {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s (confidence %.2f)\n", plain(a.ProjectID), plain(a.Suggested), a.Confidence)
		}
		return nil
	}}
	decide.Flags().StringVar(&decisionPlan, "plan", "latest", "Exact media report ID")
	decide.Flags().StringVar(&key, "key-file", "", "Explicit owner-only OpenRouter key file (required); not a Ditto key")
	_ = decide.MarkFlagRequired("key-file")
	var imagePlan, imageID, imageConsent, imageKey string
	image := &cobra.Command{Use: "image-decision", Short: "Opt-in: send ONE reencoded PNG's pixels to OpenRouter/Jev for suggestions only", Long: "Sends private screenshot pixels to OpenRouter (and Jev). Reencoding removes metadata, NOT visible private information. Only exact selected PNGs with supported dimensions are accepted. Embedded image text is untrusted. One bounded request; no actions, archival or deletion.", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := e.LoadMedia(imagePlan)
		if err != nil {
			return err
		}
		r, err := e.DecideMediaImage(cmd.Context(), p, imageID, imageConsent, imageKey)
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(cmd, r)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Jev image suggestions only. Local original retained; no archival or deletion approved.")
		for _, a := range r.Answers {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s (confidence %.2f)\n", plain(a.ProjectID), plain(a.Suggested), a.Confidence)
		}
		return nil
	}}
	image.Flags().StringVar(&imagePlan, "plan", "latest", "Exact immutable media plan (less than 24 hours old)")
	image.Flags().StringVar(&imageID, "file", "", "One exact PNG file ID from media review --details")
	image.Flags().StringVar(&imageConsent, "send-image-to-openrouter", "", "Repeat exact file ID to consent to uploading its PRIVATE PIXELS to OpenRouter/Jev")
	image.Flags().StringVar(&imageKey, "key-file", "", "Owner-only OpenRouter credential file reference; no credential values on command line")
	for _, flag := range []string{"file", "send-image-to-openrouter", "key-file"} {
		_ = image.MarkFlagRequired(flag)
	}
	root.AddCommand(scan, review, triage, decide, image, mediaDriveCommand(load, asJSON))
	return root
}
func renderMedia(w io.Writer, p disk.MediaPlan, details bool) {
	fmt.Fprintln(styled(w, toneHeading), "\nMEDIA REVIEW | Nothing deleted or uploaded")
	fmt.Fprintf(w, "%d files · %s inventoried · %s extra byte-identical copies\n", len(p.Files), size(p.Bytes), size(p.DuplicateCopyBytes))
	fmt.Fprintln(w, "These are review opportunities, not approved or guaranteed reclaimed bytes. Unique screenshots may be valuable; age is not disposal proof.")
	if !p.Complete {
		fmt.Fprintln(styled(w, toneWarning), "PARTIAL INVENTORY — keep every file; narrow --root or raise limits.")
	}
	limit := 20
	if details {
		limit = len(p.Files)
	}
	for i, c := range p.Files {
		if i >= limit {
			break
		}
		status := strings.ReplaceAll(c.Suggested, "_", " ")
		if len(c.Blockers) > 0 {
			status = "KEEP — " + strings.ReplaceAll(c.Blockers[0], "_", " ")
		}
		name := filepath.Base(c.Path)
		fmt.Fprintf(w, "\n  %8s  %-16s  %q\n", size(c.Bytes), plain(c.Category), name)
		fmt.Fprintf(w, "            %s · %d days old\n", plain(status), max(0, int(time.Since(c.Modified).Hours()/24)))
		if details {
			fmt.Fprintf(w, "            ID %s · path %q\n", plain(c.ID), c.Path)
			if c.DuplicateOf != "" {
				fmt.Fprintf(w, "            Exact-copy peer to retain: %s\n", plain(c.DuplicateOf))
			}
		}
	}
	for _, warning := range p.Warnings {
		fmt.Fprintln(w, "  Note: "+plain(warning))
	}
	if len(p.Excluded) > 0 {
		fmt.Fprintf(w, "  %d protected folders explicitly excluded; no contents read or savings counted.\n", len(p.Excluded))
		if details {
			for _, x := range p.Excluded {
				fmt.Fprintf(w, "    Excluded: %q (%s)\n", x.Path, plain(x.Reason))
			}
		}
	}
	fmt.Fprintf(w, "\nMedia plan: %s\nNo media deletion command exists. Archive upload needs a separate exact destination/file plan and explicit approval.\n", plain(p.ID))
}
func renderMediaAdvice(w io.Writer, a disk.MediaAdvice) {
	fmt.Fprintln(styled(w, toneHeading), "\nPI MEDIA TRIAGE | Suggestions only")
	if a.Run != nil {
		fmt.Fprintf(w, "%s / %s · %d requests · %d files · %d suggestions\n", plain(a.Run.Provider), plain(a.Run.Model), a.Run.Requests, a.Run.Projects, a.Run.Advised)
	}
	for _, r := range a.Recommendations {
		fmt.Fprintf(w, "  %s  %s: %q\n", plain(r.CandidateID), plain(r.Decision), plain(r.Reason))
	}
	fmt.Fprintln(w, "No images, OCR, filenames or local paths were sent. No approval, upload or deletion.")
}

func mediaDriveCommand(load func() (*disk.Engine, error), asJSON *bool) *cobra.Command {
	root := &cobra.Command{Use: "drive", Short: "Discover Google Drive and explicitly archive copies; never remove originals"}
	emit := func(cmd *cobra.Command, v any) error { return json.NewEncoder(cmd.OutOrStdout()).Encode(v) }
	discover := &cobra.Command{Use: "discover", Short: "Detect Drive Desktop accounts and gws locally; not backup verification", RunE: func(cmd *cobra.Command, _ []string) error {
		r, err := disk.DiscoverMediaDrive()
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(cmd, r)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Drive Desktop installed: %t · gws installed: %t\n", r.DesktopInstalled, r.GWSInstalled)
		for _, a := range r.Accounts {
			fmt.Fprintf(cmd.OutOrStdout(), "  %q  %q\n", a.Account, a.Mount)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Mounted files/placeholder presence is not remote backup verification. Select an account and existing destination explicitly.")
		return nil
	}}
	var account, shared string
	var listDrives bool
	folders := &cobra.Command{Use: "destinations", Short: "List remote folder IDs or Shared Drives under an explicitly verified gws account", RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := disk.NewGWSDrive(cmd.Context(), account)
		if err != nil {
			return err
		}
		var options []disk.DriveFolderOption
		if listDrives {
			options, err = d.SharedDrives(cmd.Context())
		} else {
			options, err = d.Destinations(cmd.Context(), shared)
		}
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(cmd, options)
		}
		for _, o := range options {
			fmt.Fprintf(cmd.OutOrStdout(), "%q  %q\n", o.ID, o.Name)
		}
		return nil
	}}
	folders.Flags().StringVar(&account, "account", "", "Explicit gws account email; independently checked with Drive about.user")
	folders.Flags().StringVar(&shared, "shared-drive", "", "Limit folder listing to this exact Shared Drive ID")
	folders.Flags().BoolVar(&listDrives, "list-shared-drives", false, "List accessible Shared Drive IDs instead of folders")
	_ = folders.MarkFlagRequired("account")
	var mediaPlan, archiveAccount, folderID, sharedID, visibility string
	var selected []string
	plan := &cobra.Command{Use: "plan", Short: "Plan copying exact local file IDs to an existing verified Drive folder; upload nothing", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := e.LoadMedia(mediaPlan)
		if err != nil {
			return err
		}
		d, err := disk.NewGWSDrive(cmd.Context(), archiveAccount)
		if err != nil {
			return err
		}
		a, err := e.PlanMediaArchive(cmd.Context(), p, selected, disk.DriveDestination{Account: archiveAccount, FolderID: folderID, SharedDriveID: sharedID, Visibility: visibility}, d)
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(cmd, a)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "ARCHIVE PROPOSAL — upload nothing yet\nDestination: %s\n", plain(a.Destination.String()))
		for _, grant := range a.Audience {
			fmt.Fprintf(cmd.OutOrStdout(), "  Audience: %s %q (%s)\n", plain(grant.Type), grant.Email, plain(grant.Role))
		}
		for _, f := range a.Files {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s  %q\n", f.ID, size(f.Bytes), f.Path)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Archive plan: %s\nExplicit upload approval is separate; originals are retained even after remote restore verification. No folder/ACL change or deletion.\n", a.ID)
		return nil
	}}
	plan.Flags().StringVar(&mediaPlan, "plan", "latest", "Exact media report ID")
	plan.Flags().StringSliceVar(&selected, "file", nil, "Exact file IDs to archive (required, repeatable, at most 20)")
	plan.Flags().StringVar(&archiveAccount, "account", "", "Explicit gws account email")
	plan.Flags().StringVar(&folderID, "folder", "", "Existing destination folder ID; no automatic folder creation")
	plan.Flags().StringVar(&sharedID, "shared-drive", "", "Exact engineering Shared Drive ID, if explicitly selected")
	plan.Flags().StringVar(&visibility, "visibility", "private", "private or engineering-shared; never infer destination from screenshots")
	for _, f := range []string{"file", "account", "folder"} {
		_ = plan.MarkFlagRequired(f)
	}
	var archiveID, approval string
	var ack bool
	upload := &cobra.Command{Use: "upload", Short: "Explicitly upload only an exact archive plan, download/hash verify; retain every original", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := e.LoadMediaArchive(archiveID)
		if err != nil {
			return err
		}
		if approval != p.ID {
			return errors.New("--approve-plan must equal the exact archive plan ID; this authorizes uploads, not deletion")
		}
		d, err := disk.NewGWSDrive(cmd.Context(), p.Destination.Account)
		if err != nil {
			return err
		}
		r, err := e.UploadMediaArchive(cmd.Context(), p, approval, ack, d)
		if *asJSON && r.PlanID != "" {
			_ = emit(cmd, r)
		}
		if err != nil {
			return err
		}
		if !*asJSON {
			fmt.Fprintf(cmd.OutOrStdout(), "%d remote copies download/hash verified. All local originals retained. No deletion approved.\n", len(r.Files))
		}
		return nil
	}}
	upload.Flags().StringVar(&archiveID, "archive", "", "Exact archive plan ID")
	upload.Flags().StringVar(&approval, "approve-plan", "", "Explicit approval of this exact upload plan (never deletion)")
	upload.Flags().BoolVar(&ack, "acknowledge-shared", false, "Acknowledge these selected file contents/names will be shared with the engineering drive")
	_ = upload.MarkFlagRequired("archive")
	_ = upload.MarkFlagRequired("approve-plan")
	var verifyID string
	verify := &cobra.Command{Use: "verify", Short: "Independently download/hash remote copies from an existing receipt; retain originals", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := e.LoadMediaArchive(verifyID)
		if err != nil {
			return err
		}
		d, err := disk.NewGWSDrive(cmd.Context(), p.Destination.Account)
		if err != nil {
			return err
		}
		r, err := e.VerifyMediaArchive(cmd.Context(), p, d)
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(cmd, r)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Verified %d remote restored copies; originals retained. No deletion approved.\n", len(r.Files))
		return nil
	}}
	verify.Flags().StringVar(&verifyID, "archive", "", "Exact archive plan ID with a prior upload receipt")
	_ = verify.MarkFlagRequired("archive")
	root.AddCommand(discover, folders, plan, upload, verify)
	return root
}
