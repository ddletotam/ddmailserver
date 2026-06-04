# Linux SDK setup

The Ultralight SDK is closed-source and licensed under the Ultralight Free
SDK terms; we do not check the binaries into the repo. Each Linux dev
populates them locally.

## One-time

1. Register at <https://ultralig.ht/> and grab the **Free SDK** download.
2. Pick **Linux x64** (`ultralight-free-sdk-1.4.0-linux-x64.7z`).
   Version must match the headers shipped by the `ultralight` crate —
   currently 1.4.0.
3. Unpack the archive:

   ```bash
   7z x -o/tmp/ultralight-unpack ultralight-free-sdk-1.4.0-linux-x64.7z
   ```

4. Drop the libs and resources into the crate:

   ```bash
   cd desktop/native
   mkdir -p ultralight-lib assets/resources
   cp /tmp/ultralight-unpack/bin/lib*.so   ultralight-lib/
   cp /tmp/ultralight-unpack/resources/*   assets/resources/
   ```

5. Build & run:

   ```bash
   cargo run
   ```

Both `ultralight-lib/` and `assets/resources/` are gitignored.

## How it works

`build.rs` adds `ultralight-lib/` to the linker search path on Linux and
bakes an `RPATH` of `$ORIGIN/../../ultralight-lib` into the binary so
`cargo run` works without `LD_LIBRARY_PATH`. `RUNPATH` (new dtags) is
also set, so `LD_LIBRARY_PATH` can still override at runtime for testing
alt SDK builds.

`render.rs` calls `ultralight::init("desktop/native/assets", None)` and
sets `resource_path_prefix = "resources/"`, so the `.dat` and `.pem`
resources must live under `assets/resources/`.

## Windows / macOS

The upstream `ultralight` crate ships Windows `.dll`s and `.lib`s in its
own `ultralight-bin/` and `ultralight-lib/`; that path is the default
when `feature = "requires_dll"` is enabled, which is what we use today.
macOS is not yet supported — would need similar one-time setup against
`ultralight-sdk-1.4.0-mac-x64.7z`.

## License note

The Ultralight Free SDK is free for products with under $100k/year
revenue. Above that, a commercial license is required. The downloaded
files are personal to the developer who registered for them and must
not be redistributed; that's why they're gitignored.
