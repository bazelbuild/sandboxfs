# sandboxfs: A virtual file system for sandboxing

Sandboxfs is an open-source FUSE file system that exposes a combination of
multiple files and directories from the host's file systems in the form of a
virtual tree with an arbitrary layout.

Sandboxfs is designed to allow running sandboxed commands with limited access
to the file system by using the virtual tree as their new root, and to do so
consistently across a variety of platforms.

Sandboxfs is licensed under the [Apache 2.0 license](LICENSE) and is not an
official Google product.

Currently it can map multiple readonly files and directories in form of an
arbitrary tree, at a specified mount point.
## Usage

```
sandboxfs [generic flags...] command [command flags]...
```

The following generic flags are available:

`--debug`
: Log debugging information (received requests and responses from server) to
  stderr.

`--help`
: Print global help information and exit.

`--volume_name=VOLUME-NAME`
: Name of the mounted volume. Default: sandboxfs.

## Commands

### static

```
sandboxfs [generic flags...] static [command flags...]
```

Statically configured sandbox using command line flags.
A static sandbox may be configured with the following flags:

`--help`
: Print `static` command help information and exit.

`--read_only_mapping=MAPPING:TARGET`
: Add a new read-only mapping to the sandboxfs configuration. Multiple
  mappings may be added by giving this flag multiple times with different
  arguments. Mappings may be nested on top of each other, and target map point
  to both files and directories.  MAPPING is the path of the node inside the
  mount, relative to the mountpoint root.
  TARGET is the path of the node in the existing filesystem that needs to be
  mapped.

`--read_write_mapping=MAPPING:TARGET`
: Add a new read/write mapping to the sandboxfs configuration. It follows the
  same configuration rules as `--read_only_mapping`, except that the resulting
  mapped directories can be written to. `--read_only_mapping` and
  `--read_write_mapping` can be used together in a single tree.

### dynamic

```
sandboxfs [generic flags...] dynamic [command flags...]
```

Sandbox is configured dynamically by providing the configuration in JSON, to
the input of the sandboxfs process. An unconfigured dynamic sandbox is empty.
If a valid configuration is received, the program writes `Done` to the output,
after reconfiguring the file system under the mountpoint. A JSON config would
look like:

```
{
    "read_write_mapping": [
        {
            "Mapping": "/alpha/beta",
            "Target": "/root"
        }
    ],
    "read_only_mapping": [
        {
            "Mapping": "/beta",
            "Target": "/usr/bin/bash"
        }
    ]
}
```

A dynamic sandbox may be configured with the following flags:

`--help`
: Print `dynamic` command help information and exit.

`--input`
: File to read the configuration data from (default is "-" for stdin).

`--output`
: File to write the status of reconfiguration to (default is "-" for stdout).

