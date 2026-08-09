//! Does rustls actually talk to these mail servers?
//!
//! The stack was moved off the platform TLS library, and rustls is stricter:
//! TLS 1.2 and 1.3 only, and a certificate must carry a subjectAltName. A host
//! stuck on TLS 1.0/1.1 or serving a CN-only certificate connected before and
//! will not now. This says which, without touching a password.
//!
//! `cargo run --example tls_probe -- mail.example.ru:993 smtp.example.ru:465`

use futures_rustls::TlsConnector;
use tokio::net::TcpStream;
use tokio_util::compat::TokioAsyncReadCompatExt;

#[tokio::main]
async fn main() {
    ddmail_core::tls::init();
    let targets: Vec<String> = std::env::args().skip(1).collect();
    if targets.is_empty() {
        eprintln!("usage: tls_probe host:port [host:port ...]");
        return;
    }
    let mut failed = false;
    for target in targets {
        let (host, port) = match target.rsplit_once(':') {
            Some((h, p)) => (h.to_string(), p.parse::<u16>().unwrap_or(993)),
            None => (target.clone(), 993),
        };
        print!("{host}:{port} ... ");
        let result = handshake(&host, port).await;
        match result {
            Ok(()) => println!("ok"),
            Err(e) => {
                failed = true;
                println!("FAILED: {e}");
            }
        }
    }
    if failed {
        std::process::exit(1);
    }
}

async fn handshake(host: &str, port: u16) -> Result<(), String> {
    let tcp = TcpStream::connect((host, port)).await.map_err(|e| format!("TCP: {e}"))?;
    let name = ddmail_core::tls::server_name(host)?;
    ddmail_core::tls::connector()
        .connect(name, tcp.compat())
        .await
        .map(|_| ())
        .map_err(|e| format!("TLS: {e}"))
}
