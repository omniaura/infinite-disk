# Screenshots and Downloads: review, then preserve

This is a first-class CLI/JSON workflow, not a one-off cleanup script. Nothing in
`media scan`, `review`, `triage`, `decide` or `image-decision` approves deletion,
uploads originals to Drive, or modifies originals. No media delete/trash command
exists in this release. AI suggestions cannot grant consent or execute actions.

## Local report

```sh
infinite-disk media scan --root "$HOME/Downloads" --limit 5000 --hash-mib 2048
infinite-disk media review --details
infinite-disk media review --plan MEDIA_PLAN --json
```

Defaults are existing `~/Downloads`, `~/Desktop`, and `~/Pictures/Screenshots`.
Explicit roots must stay inside Downloads/Desktop/Pictures; this does **not**
unprotect those locations for project removal. A narrowed `--root` is useful for
large folders. Plans are immutable and stored privately in a separate
`media-plans/` namespace; they cannot be consumed by project `apply`.

Files are sorted largest first and show age, category, review reason, duplicate
peer ID, and hold reasons. Categories come from extensions and screenshot-name
heuristics, **not visual inspection**. A unique screenshot, old document,
installer, archive or video is not presumed disposable. Age is only a review
signal. Exact duplicate groups require native SHA256 of equal-sized files;
one designated peer stays. The duplicate-copy total is logical bytes, not
guaranteed APFS net savings. Sparse/offline files are not read or hydrated.
Content reads are also restricted to qualified local filesystems (macOS APFS/HFS;
Linux ext/XFS/Btrfs/tmpfs/overlay). Unknown, network and FUSE mounts stay held.

The scanner holds symlinks, hardlinks, special files, unowned files, hidden/private
paths, recently modified/open files, unknown activity, and partial downloads.
Hidden/package/vendor directories are explicitly outside the report's scope,
listed as exclusions without reading contents or counting savings. They do not
block unrelated fully enumerated files. A report is complete only for its declared
scope, never a claim that excluded folders were inventoried or safely disposable.
Incomplete enumeration, changing directories, depth/entry limits or cancellation
cannot become a safe complete inventory. Partial reports hold every file and
cannot create archive plans; narrow the root rather than bypassing checks.
The default is 5,000 files, depth 6, 2 GiB total duplicate hashing, 128 MiB maximum
hashed file and five minutes. Maximum limits are 20,000 files/depth 12/16 GiB.
Large unique media is metadata-only until explicitly selected for a bounded
archive. Reports are snapshots, not live state; native operations recheck files.

## Advisory inference and privacy

```sh
# Reuses agent setup/configure and the selected Ditto/OpenRouter profile:
infinite-disk agent setup # update the embedded worker after upgrading Infinite Disk
infinite-disk media triage --plan MEDIA_PLAN --json
# Or inventory then triage, publishing the inventory even if inference fails:
infinite-disk media scan --root "$HOME/Downloads" --agent --json
# One direct typed Jev request; does not change your configured Pi profile:
infinite-disk media decide --plan MEDIA_PLAN --key-file /private/openrouter-key --json
```

Default AI input is an explicit projection: opaque ID, byte size, age,
complete/held status, fixed hold codes, category, and native duplicate/hash facts.
No filename, path, image pixels, OCR, document contents, EXIF/GPS, hashes or freeform
file metadata is sent. Pi has only inventory/inspection/suggestion tools: no
filesystem, shell, upload, approval or deletion tools. Pi permits four requests,
4,096 output tokens/request, no retries and a bounded runtime. Jev metadata uses
one pinned `typesafe/jev-1.14` Decisions request, 30 seconds, 32 KiB input and
512 KiB response. Typed enum/probability/coverage/model/usage validation remains
native. Low confidence escalates; a confident suggestion is still not consent.
Run receipts record provider/model, calls, token usage when reported, and
`approved:false`/`executed:false`. `--agent --json` emits inventory then advice as
two JSON Lines. No command enables scheduled paid inference. Request/token/time
limits are not a hard dollar cap; provider-reported billing remains authoritative.

### Opt-in single-PNG visual suggestion

```sh
infinite-disk media image-decision --plan MEDIA_PLAN --file FILE_ID \
  --send-image-to-openrouter FILE_ID --key-file /private/openrouter-key --json
```

**This sends private screenshot pixels to OpenRouter and Jev.** Visible private
text remains private content even though separate OCR is not performed. Use only
after reviewing that exact file locally. The consent flag must repeat its opaque
file ID from the exact report, which must be complete and less than 24 hours old.
There is no bulk, default or scheduler vision mode. Names, paths, OCR fields,
and original PNG ancillary metadata are never included. Native decoding and
reencoding strips PNG metadata; it does not anonymize the pixels.

The narrow experiment accepts one local PNG up to 8 MiB, each dimension
256..1568, area 78,400..2,458,624 pixels, no animation, and no resize. Unsupported
JPEG/HEIC/PDF/other formats fail explicitly. Native activity/identity/hash checks
run before and after reading; cancellation prevents inference. It makes one
pinned Jev request with top-level Decisions image parts, maximum 12 MiB request,
512 KiB response, 30-second network timeout and no retries/redirects. PNG decoding
is locally bounded by size/pixels, but is not claimed to be OS-sandboxed.
Image text is untrusted, never instructions. Only `keep`, `review_archive`,
`review_disposable`, or `escalate` can be returned; none applies anything.
The private decision receipt labels this input `png_pixels`; metadata decisions
are labeled `metadata`. No real user pixels were used in the fixture test suite.
Ditto Router chat compatibility is **not** assumed to implement Decisions/image
Decisions. This experiment goes directly to OpenRouter with a separate key file.

## Google Drive discovery and explicit archival

```sh
infinite-disk media drive discover --json
infinite-disk media drive destinations --account you@example.com --json
infinite-disk media drive destinations --account you@example.com --list-shared-drives --json
infinite-disk media drive destinations --account you@example.com --shared-drive DRIVE_ID --json
infinite-disk media drive plan --plan MEDIA_PLAN --file FILE_ID,OTHER_ID \
  --account you@example.com --folder FOLDER_ID --visibility private --json
# Read the exact plan before approving the transfer:
infinite-disk media drive upload --archive ARCHIVE_PLAN --approve-plan ARCHIVE_PLAN --json
infinite-disk media drive verify --archive ARCHIVE_PLAN --json
```

Install/authenticate the Google Workspace CLI (`gws`) separately. Infinite Disk
does not install it, export tokens, sign into accounts or change defaults. Because
multiaccount behavior has changed upstream, the installed CLI must advertise
`GOOGLE_WORKSPACE_CLI_ACCOUNT`; unsupported interfaces fail explicitly. Every
operation independently checks Drive `about.user.emailAddress`, not just that
selector. Environment credential overrides are stripped from the subprocess.
Drive Desktop discovery only reads account mount names. A mount, local copy or
placeholder is **never remote backup proof**, and discovery never hydrates files.

No account or destination is selected globally/defaulted. Private archives require
an existing unshared folder, owned only by the selected account, with no extra
permissions. Ambiguous/incomplete permissions fail. The tool does not create
folders or alter ACLs. For engineering shared storage, explicitly add
`--visibility engineering-shared --shared-drive DRIVE_ID` when planning and
`--acknowledge-shared` when uploading. **Selected original names and file contents
will then be visible to that shared drive's members.** Never infer a shared
destination from a screenshot or account name. Folder ID, actual drive ID, account
and write capability are checked and bound into the immutable plan.
Folder and uploaded-file audiences are independently enumerated with bounded,
complete `permissions.list` pagination (not the omitted Shared Drive
`files.permissions` field). Empty/unknown, anyone/public and domain grants fail
closed. Explicit user/group grants and roles are shown in the approval plan;
Shared Drive inherited/member details must be available. ACL ordering is
canonicalized. The restored file's audience must match the approved folder's
user/group audience; a changed grant, role or folder-permission digest fails.

The transfer slice is 1..20 explicitly selected files, at most 128 MiB each and
512 MiB/batch. Each source is hashed and rechecked. An archive plan expires for
upload after 24 hours. Upload requires repeating its full plan ID; AI cannot
perform or approve it. Original names/content go to Drive only at this explicit
archive stage, not during metadata triage. A private local staging copy is used,
then independent remote download restores to temporary scratch and verifies
SHA256, byte size, remote identity/version, MIME, parent, private visibility (for
private plans), selected shared drive and plan-specific app properties. All
originals remain. Only the tool's own temporary staging/restored scratch is
discarded. Allow roughly twice one file's size for scratch; remote transfer has
a per-subprocess two-minute deadline. Downloads are initiated only after bounded
remote-size checks; this CLI adapter is not a streaming OS disk quota.

IDs are reserved before upload and recorded in a private recovery receipt.
Uploads use `application/octet-stream` (no Google Workspace conversion), explicit
`supportsAllDrives`, and default-visibility override; inherited parent sharing is
still checked. Unknown/interrupted outcomes retain originals and IDs. Repeating
upload is refused, so it cannot silently create duplicates; `verify` can reconcile
an upload that completed remotely before a local interruption. Missing or changed
remote copies, wrong identity, truncated/corrupt downloads and permission changes
fail closed. Multi-file resume/retry and removal of verified local originals are
deliberately unsupported in this slice. There is no claim that every real gws
version, account, Shared Drive policy, quota, or network failure has been live-tested.

Versioned JSON plans/receipts are intended for Ditto Desktop consent/review, not
as permission to execute model-proposed commands. Plans and receipts are private
local state; backup/archive copies are not encrypted by Infinite Disk in this
workflow. Native media Trash/restore receipts, broader formats, batched visual
consent, archive encryption and desktop integration need separate reviewed work.
The media roadmap is tracked in [#40](https://github.com/omniaura/infinite-disk/issues/40),
with archive, approval and desktop contracts linked to #7, #3 and #4 respectively.

## Primary protocol references

- [OpenRouter Jev typed decisions tutorial](https://openrouter.ai/blog/tutorials/how-to-use-jev/)
- [OpenRouter multimodal Decisions protocol and image limits](https://openrouter.ai/docs/guides/community/multimodal-decisions)
- [Google Workspace CLI source, auth and binary upload/download](https://github.com/googleworkspace/cli)
- [Drive uploads, pre-generated IDs and conversion restrictions](https://developers.google.com/workspace/drive/api/guides/manage-uploads#use_a_pre-generated_id_to_upload_files)
- [Drive Shared Drive support and request flags](https://developers.google.com/workspace/drive/api/guides/enable-shareddrives)
- [Drive create/default visibility](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/create)
- [Drive binary download and capabilities](https://developers.google.com/workspace/drive/api/guides/manage-downloads)

Pre-generated IDs are documented for binary create/upload and safe retry (409
on an already-created ID); the documented exclusion is Google Workspace conversion
MIME types except folder/drive-sdk. The adapter combines this binary-only path
with Shared Drive request flags; policy-specific rejection remains a failure,
not assumed success. A verified receipt requires actual independent readback.
