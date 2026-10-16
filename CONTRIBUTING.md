# Contributing

Open an issue for safety-sensitive design changes first. Small pull requests are welcome. Run the checks in README and add fixture tests for each changed safety boundary. Never test destructive workflows against your actual repositories or someone else's files. Never add provider keys, user inventories, recordings, or private recovery data to fixtures.

Approval-first behavior is invariant: scheduled work may discover and recommend; it cannot approve, commit, push, or remove. New adapters must prove backup/readback/restore independently and fail closed when activity or preservation is unknown. Distinguish proposed actions from performed steps and apparent size from measured free space.

The maintainer reviews and merges contributions through protected main. Tests must pass at the current head. MIT is the inbound and outbound license; contributing means you have permission to submit the code under MIT. Be respectful, constructive, and privacy-conscious. Harassment and posting private information are not accepted.
