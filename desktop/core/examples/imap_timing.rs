//! Where the time goes when the client fetches a message source.
//!
//! Every such request opens a fresh session (`with_session!` in
//! `imap_provider`), so the cost is connect + login + select + fetch, and the
//! fetch is `BODY.PEEK[]` — the whole message, attachments included, even when
//! all the caller wanted was the header block.
//!
//! IMAP_HOST/IMAP_PORT/IMAP_USER/IMAP_PASS/IMAP_FOLDER in the environment;
//! nothing is written and the fetches PEEK, so nothing is marked read.

use std::time::Instant;

use futures::TryStreamExt;
use tokio_util::compat::TokioAsyncReadCompatExt;

#[tokio::main]
async fn main() {
    ddmail_core::tls::init();
    let host = std::env::var("IMAP_HOST").expect("IMAP_HOST");
    let port: u16 = std::env::var("IMAP_PORT").ok().and_then(|p| p.parse().ok()).unwrap_or(993);
    let user = std::env::var("IMAP_USER").expect("IMAP_USER");
    let pass = std::env::var("IMAP_PASS").expect("IMAP_PASS");
    let folder = std::env::var("IMAP_FOLDER").unwrap_or_else(|_| "INBOX".into());

    let t = Instant::now();
    let tcp = tokio::net::TcpStream::connect((host.as_str(), port)).await.expect("tcp");
    let tls = ddmail_core::tls::connector()
        .connect(ddmail_core::tls::server_name(&host).expect("name"), tcp.compat())
        .await
        .expect("tls");
    println!("connect + TLS      {:>6} ms", t.elapsed().as_millis());

    let t = Instant::now();
    let mut session = async_imap::Client::new(tls).login(&user, &pass).await.map_err(|e| e.0).expect("login");
    println!("LOGIN              {:>6} ms", t.elapsed().as_millis());

    let t = Instant::now();
    let mailbox = session.select(&folder).await.expect("select");
    println!("SELECT             {:>6} ms  ({} messages)", t.elapsed().as_millis(), mailbox.exists);

    // Newest message: the one the source view would most likely be opened on.
    let t = Instant::now();
    let uids: Vec<u32> = session
        .uid_search("ALL")
        .await
        .expect("search")
        .into_iter()
        .collect();
    let uid = *uids.last().expect("a message");
    println!("UID SEARCH ALL     {:>6} ms  (newest uid {uid})", t.elapsed().as_millis());

    for spec in ["BODY.PEEK[HEADER]", "BODY.PEEK[]"] {
        let t = Instant::now();
        let stream = session.uid_fetch(uid.to_string(), spec).await.expect("fetch");
        let msgs: Vec<_> = stream.try_collect().await.expect("collect");
        let bytes = msgs.first().and_then(|m| m.body()).map_or(0, |b| b.len());
        println!("{spec:<18} {:>6} ms  ({} KB)", t.elapsed().as_millis(), bytes / 1024);
    }
    session.logout().await.ok();
}
