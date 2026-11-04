# Roadmap

## Next
- `--all` to also check profile edges whose caller is in the standard library or other modules, which shows drift after a Go or dependency upgrade
- Markdown output for pull request comments
- Check every main package that has a `default.pgo` in one run

## Later
- Compare the PGO inlining and devirtualization decisions of the stale and a fresh profile, using `-m=2` output
- Read `-trimpath` builds, whose call site positions don't point at files on disk
- A GitHub Action wrapper, once `check --fail` has been used in a real CI run
