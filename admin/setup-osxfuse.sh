#! /bin/sh
# Copyright 2019 Google Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License"); you may not
# use this file except in compliance with the License.  You may obtain a copy
# of the License at:
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
# WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.  See the
# License for the specific language governing permissions and limitations
# under the License.

# Startup script to load OSXFUSE and enable "-o allow_other".
#
# sandboxfs requires this setting in order for binaries mapped within the
# sandbox to work.  See the following for more details:
# http://julio.meroh.net/2017/10/fighting-execs-sandboxfs-macos.html
#
# We use a startup script instead of an entry in /etc/sysctl.conf because
# it's easier to manage installation and deinstallation, and because we
# must first ensure OSXFUSE is loaded in order to change its configuration.

/Library/Filesystems/osxfuse.fs/Contents/Resources/load_osxfuse
/usr/sbin/sysctl -w vfs.generic.osxfuse.tunables.allow_other=1
