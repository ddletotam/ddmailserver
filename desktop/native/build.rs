fn main() {
    slint_build::compile("ui/app.slint").unwrap();

    // Linux build path for Ultralight.
    //
    // The upstream `ultralight` crate's build.rs is Windows-oriented: it
    // points the linker at its bundled `ultralight-lib/` which contains
    // MSVC `.lib` files, then copies DLLs to the target dir. On Linux we
    // override that by adding our own `ultralight-lib/` (alongside this
    // build.rs) containing the four `.so`s from the official 1.4.0 Linux
    // SDK, and bake an RPATH so `cargo run` finds them without
    // LD_LIBRARY_PATH gymnastics.
    //
    // The SDK files aren't checked in (Ultralight Free SDK license is
    // not open source); see desktop/native/README-LINUX-SDK.md for the
    // one-time steps to populate `ultralight-lib/` and `assets/resources/`.
    #[cfg(target_os = "linux")]
    {
        let lib_dir = format!("{}/ultralight-lib", env!("CARGO_MANIFEST_DIR"));
        println!("cargo:rerun-if-changed=ultralight-lib");
        println!("cargo:rustc-link-search=native={lib_dir}");
        // RPATH relative to the binary at target/{profile}/ddmail-native →
        // up two levels gets us to desktop/native, then into ultralight-lib.
        // RUNPATH (--enable-new-dtags) lets LD_LIBRARY_PATH override at
        // runtime, which is convenient for testing alt builds.
        println!("cargo:rustc-link-arg=-Wl,-rpath,$ORIGIN/../../ultralight-lib");
        println!("cargo:rustc-link-arg=-Wl,--enable-new-dtags");
    }
}
