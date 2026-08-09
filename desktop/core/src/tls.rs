//! One rustls client configuration for every outbound TLS connection.
//!
//! rustls rather than the platform's TLS library: this ships for Windows and
//! Linux only, and a system TLS dependency (OpenSSL headers, in practice) is
//! the thing that breaks a Linux build first. Roots come from the OS store, so
//! a corporate CA installed on the machine keeps working.
//!
//! One caveat worth knowing when a server suddenly stops connecting: rustls
//! speaks TLS 1.2 and 1.3 only, and requires a certificate with a
//! subjectAltName. A mail host stuck on TLS 1.0/1.1, or serving a
//! CN-only certificate, will be refused where the old stack accepted it.

use std::sync::{Arc, Once, OnceLock};

use futures_rustls::TlsConnector;
use rustls::ClientConfig;

/// Pick the crypto backend for the whole process, once.
///
/// rustls 0.23 refuses to guess when more than one provider is compiled in, and
/// with four crates in the graph pulling rustls (ours, reqwest, lettre,
/// tungstenite) exactly which providers are enabled is decided by feature
/// unification — not something to litigate in a manifest. Naming it here is
/// deterministic, and every one of those crates builds its config off this
/// process default. Call it before any TLS happens.
pub fn init() {
    static ONCE: Once = Once::new();
    ONCE.call_once(|| {
        // `Err` only means someone got here first, which is fine.
        let _ = rustls::crypto::ring::default_provider().install_default();
    });
}

/// Shared connector — building one costs a scan of the OS certificate store.
pub fn connector() -> TlsConnector {
    static CONFIG: OnceLock<Arc<ClientConfig>> = OnceLock::new();
    init();
    let config = CONFIG.get_or_init(|| {
        let mut roots = rustls::RootCertStore::empty();
        // A machine with no usable roots is a machine that cannot verify
        // anything; carry on with an empty store and let the handshake say so,
        // rather than failing at startup for an account nobody is using.
        // `CertificateResult` reports per-store errors alongside whatever it
        // did manage to read; take the certificates and ignore the complaints.
        for cert in rustls_native_certs::load_native_certs().certs {
            let _ = roots.add(cert);
        }
        Arc::new(ClientConfig::builder().with_root_certificates(roots).with_no_client_auth())
    });
    TlsConnector::from(Arc::clone(config))
}

/// The server name a handshake needs, from a host string.
pub fn server_name(host: &str) -> Result<rustls::pki_types::ServerName<'static>, String> {
    rustls::pki_types::ServerName::try_from(host.to_string())
        .map_err(|_| format!("TLS: bad host name {host}"))
}
