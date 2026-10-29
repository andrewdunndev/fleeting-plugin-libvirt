# Contributing to fleeting-plugin-libvirt

fleeting-plugin-libvirt is a personal project; contributions are welcome, and
the bar is whether they make clones of one base volume safer to create, find
and delete. Issues and merge requests go to
[GitLab](https://gitlab.com/dunn.dev/fleeting-plugin-libvirt).

## Development environment

`mise install` provides Go, just and golangci-lint at the pinned versions. No
hypervisor is needed: tests run against a fake of the small `Client` interface.

```
just test    go test -race -p 4 ./...
just lint    golangci-lint and go vet
just build   bin/fleeting-plugin-libvirt
```

Run `just lint` and `just test` before opening a merge request.

## Conventions

- Errors say what to do next, not only what went wrong.
- The plugin deletes only what it created under its `name_prefix`, and never at Init or
  Shutdown.
- Comments are sparse and explain why, never history.
- Tests assert something that fails when the behavior breaks; check a new test
  by breaking the code under it.
