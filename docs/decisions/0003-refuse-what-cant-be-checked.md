# 0003: Refuse input that can't be checked

**Decision:** pgostale exits 2 with nothing on stdout when it can't trust its input, with or without `--fail`.

**Why:** a profile that the compiler ignores, such as an empty file, a profile cut off while being written, a heap profile, or a profile from a crashed run, must never read as "still matches". Neither may a build that failed, which would leave no call sites to compare. A profile where nothing in the module matches is refused too, because it is either from another program or so stale that the list of losses says nothing useful.

**Details:** gzip's checksum catches any cut in a gzipped profile. Uncompressed profiles are checked for missing strings, locations and functions. Tests cut a real profile at every byte and require either a refusal or exactly the edges of the whole file. The `go tool preprofile` text format has no end marker, so a file cut at a record boundary looks complete. It is refused, with a message asking for the pprof file it was made from.
