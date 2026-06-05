fn main() {
    slint_build::compile("ui/app.slint").unwrap();
    // WebKitGTK / GTK / Cairo are pulled in through pkg-config via their
    // respective crates' build scripts — no extra wiring needed here.
}
