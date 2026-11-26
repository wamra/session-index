# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's **Security** tab and
choose **Report a vulnerability**. Do not open a public issue for a security problem.

Include the version (`session-index version`), your operating system, and the steps to reproduce.
Do not attach real transcripts or archive files; the archive holds everything said in your Claude Code
sessions, so describe the problem or use a synthetic example instead.

## What to expect

This is a small project maintained in spare time. Reports are read and answered as soon as possible,
with no guaranteed response time.

## Scope notes

- The archive stores transcripts as they are, including any secrets that appear in them (see
  "Privacy and security" in the README). That is documented behaviour, not a vulnerability.
- Encryption protects the archive file at rest only; see `docs/REFERENCE.md`, Encryption, for what it
  does and does not cover.
