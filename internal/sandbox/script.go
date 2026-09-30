// Package sandbox supplies file hooks without modifying AgentBox templates.
package sandbox

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"sync"
)

//go:embed files.py
var source []byte

// Digest pins sandbox code in the workflow's effective revision.
func Digest() string { return fmt.Sprintf("%x", sha256.Sum256(source)) }

func Path() string { return ".orpheus/mattermost/code/" + Digest() + ".py" }

// Bootstrap runs with -c, leaving stdin available for binary clarification data.
// Code is installed atomically under its content hash and never replaces another version.
var Bootstrap = sync.OnceValue(func() string {
	var packed bytes.Buffer
	writer := zlib.NewWriter(&packed)
	_, _ = writer.Write(source)
	_ = writer.Close()
	return fmt.Sprintf(`import base64,hashlib,os,sys,zlib
source=zlib.decompress(base64.b64decode(%q))
assert hashlib.sha256(source).hexdigest()==%q
ns={"__name__":"mattermost_files"}
exec(compile(source,%q,"exec"),ns)
try:
    with ns["step"]("script_installation"):
        store=ns["Store"](os.environ["ORPHEUS_WORKSPACE_PATH"])
        try:
            store.write(%q,source)
        finally:
            store.close()
    ns["main"](sys.argv[1])
except Exception as error:
    ns["report_failure"](error)
    sys.exit(1)
`, base64.StdEncoding.EncodeToString(packed.Bytes()), Digest(), Path(), Path())
})

func BeforeRun() string {
	return "#!/bin/sh\nset -eu\nexec python3 -I -c '" + Bootstrap() + "' prepare-input\n"
}

// AfterRun verifies the installed bytes before executing the session's pinned code.
func AfterRun() string {
	code := fmt.Sprintf(`import errno,hashlib,os,sys
try:
    name=os.path.join(os.environ["ORPHEUS_WORKSPACE_PATH"],%q)
    with open(name,"rb") as stream:
        source=stream.read(65537)
except OSError as error:
    kind=type(error).__name__
    code=error.errno
    label=errno.errorcode.get(code,"UNKNOWN")
    print("Mattermost file hook failed: stage=script_loading; %%s: errno=%%s (%%s)" %% (kind,code,label),file=sys.stderr)
    sys.exit(1)
except KeyError:
    sys.exit("Mattermost file hook failed: stage=script_loading; ORPHEUS_WORKSPACE_PATH is missing")
if hashlib.sha256(source).hexdigest()!=%q:
    sys.exit("Mattermost file hook failed: stage=script_integrity; script checksum mismatch")
exec(compile(source,name,"exec"))
`, Path(), Digest())
	return "#!/bin/sh\nset -eu\nexec python3 -I -c '" + code + "' export-output\n"
}
