# Major changes between releases

## Changes in version 0.1.1

**Released 2019-10-24.**

* Fixed the definition of `--input` and `--output` to require an argument,
  which makes `--foo bar` and `--foo=bar` equivalent.  This can be thought to
  break backwards compatibility but, in reality, it does not.  The previous
  behavior was just broken: specifying `--foo bar` would cause `bar` to be
  treated as an argument and `--foo` to use its default value, which meant
  that these two flags would be ignored when supplied under this syntax.

* Fixed `--input` and `--output` to handle stdin and stdout correctly when
  running e.g. under `sudo`.

* Make create operations honor the UID and GID of the caller user instead of
  inheriting the permissions of whoever was running sandboxfs.  Only has an
  effect when using `--allow=other` or `--allow=root`.

## Changes in version 0.1.0

**Released on 2019-02-05.**

This is the first formal release of the sandboxfs project.

**WARNING:** The interaction points with sandboxfs are subject to change at this
point.  In particular, the command-line interface and the data format used to
reconfigure sandboxfs while it's running *will* most certainly change.
