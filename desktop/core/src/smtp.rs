use std::sync::atomic::{AtomicU64, Ordering};
use lettre::{
    transport::smtp::authentication::Credentials,
    AsyncSmtpTransport, AsyncTransport, Message, Tokio1Executor,
};
use lettre::message::{header::ContentType, Attachment, Mailbox, MultiPart, SinglePart};

use crate::types::{OutgoingAttachment, OutgoingMessage};

// RFC 5322 §3.6.4 says Message-ID SHOULD be unique. lettre 0.11 doesn't add one
// unless we call `.message_id(...)` explicitly — so without this every send goes out
// without a Message-ID and our server has to mint a `<unixnano@generated.local>`
// fallback, which makes the same email impossible to dedup across folders.
static MSG_ID_COUNTER: AtomicU64 = AtomicU64::new(0);

fn make_message_id(from_email: &str) -> String {
    let domain = from_email.rsplit_once('@').map(|(_, d)| d).unwrap_or("localhost");
    let nanos = chrono::Utc::now().timestamp_nanos_opt().unwrap_or(0) as u64;
    let counter = MSG_ID_COUNTER.fetch_add(1, Ordering::Relaxed);
    format!("<{nanos:x}.{counter:x}@{domain}>")
}

/// Core send logic, reusable by the `send_message` wrapper and ImapProvider.
pub(crate) async fn send_message_impl(
    host: &str, port: u16, username: &str, password: &str, use_tls: bool,
    message: &OutgoingMessage,
) -> Result<String, String> {
    let from_mailbox: Mailbox = message.from.parse()
        .map_err(|e| format!("Invalid from address: {e}"))?;

    let msg_id = make_message_id(from_mailbox.email.as_ref());
    let mut email_builder = Message::builder()
        .from(from_mailbox)
        .message_id(Some(msg_id))
        .subject(&message.subject);

    for to_addr in &message.to {
        let mailbox: Mailbox = to_addr.parse()
            .map_err(|e| format!("Invalid to address '{to_addr}': {e}"))?;
        email_builder = email_builder.to(mailbox);
    }

    for cc_addr in &message.cc {
        let mailbox: Mailbox = cc_addr.parse()
            .map_err(|e| format!("Invalid cc address '{cc_addr}': {e}"))?;
        email_builder = email_builder.cc(mailbox);
    }

    if let Some(ref reply_to) = message.in_reply_to {
        email_builder = email_builder.in_reply_to(reply_to.clone());
    }

    if let Some(ref refs) = message.references {
        email_builder = email_builder.references(refs.clone());
    }

    let mp = build_body(&message.text, &message.html, &message.attachments)?;

    let email = email_builder
        .multipart(mp)
        .map_err(|e| format!("Build email: {e}"))?;

    let creds = Credentials::new(username.to_string(), password.to_string());

    let mailer = if use_tls {
        AsyncSmtpTransport::<Tokio1Executor>::relay(host)
            .map_err(|e| format!("SMTP relay: {e}"))?
            .port(port)
            .credentials(creds)
            .build()
    } else {
        AsyncSmtpTransport::<Tokio1Executor>::starttls_relay(host)
            .map_err(|e| format!("SMTP starttls: {e}"))?
            .port(port)
            .credentials(creds)
            .build()
    };

    let response = mailer.send(email)
        .await
        .map_err(|e| format!("Send failed: {e}"))?;

    Ok(format!("Sent: {:?}", response.code()))
}

/// Convenience wrapper over `send_message_impl`.
pub async fn send_message(
    host: String, port: u16, username: String, password: String, use_tls: bool,
    message: OutgoingMessage,
) -> Result<String, String> {
    send_message_impl(&host, port, &username, &password, use_tls, &message).await
}

/// Assemble the message body part. Layered MIME structure:
///   - body_alt = multipart/alternative { text/plain, text/html }
///   - if any inline attachments → multipart/related { body_alt, ...inlines }
///   - if any file attachments    → multipart/mixed  { related-or-alt, ...files }
///
/// Inline parts carry Content-ID so the HTML can reference them as cid:….
/// File parts use Content-Disposition: attachment.
fn build_body(
    text: &str,
    html: &str,
    attachments: &[OutgoingAttachment],
) -> Result<MultiPart, String> {
    let body_alt = MultiPart::alternative()
        .singlepart(
            SinglePart::builder()
                .header(ContentType::TEXT_PLAIN)
                .body(text.to_string()),
        )
        .singlepart(
            SinglePart::builder()
                .header(ContentType::TEXT_HTML)
                .body(html.to_string()),
        );

    let (inlines, files): (Vec<_>, Vec<_>) = attachments
        .iter()
        .partition(|a| a.content_id.is_some());

    // Wrap body_alt with multipart/related when there are inline images, so the
    // HTML's cid: references are resolved against the same envelope.
    let body_or_related = if inlines.is_empty() {
        body_alt
    } else {
        let mut related = MultiPart::related().multipart(body_alt);
        for att in &inlines {
            related = related.singlepart(build_inline_part(att)?);
        }
        related
    };

    if files.is_empty() {
        return Ok(body_or_related);
    }

    let mut mixed = MultiPart::mixed().multipart(body_or_related);
    for att in &files {
        mixed = mixed.singlepart(build_file_part(att)?);
    }
    Ok(mixed)
}

fn build_inline_part(att: &OutgoingAttachment) -> Result<SinglePart, String> {
    let ct: ContentType = att.mime_type.parse()
        .map_err(|_| format!("invalid mime for inline {}", att.filename))?;
    let cid = att.content_id.as_deref().unwrap_or_default();
    Ok(Attachment::new_inline(cid.to_string()).body(att.content.clone(), ct))
}

fn build_file_part(att: &OutgoingAttachment) -> Result<SinglePart, String> {
    let ct: ContentType = att.mime_type.parse()
        .map_err(|_| format!("invalid mime for {}", att.filename))?;
    Ok(Attachment::new(att.filename.clone()).body(att.content.clone(), ct))
}
