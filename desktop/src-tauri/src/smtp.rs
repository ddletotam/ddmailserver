use lettre::{
    transport::smtp::authentication::Credentials,
    AsyncSmtpTransport, AsyncTransport, Message, Tokio1Executor,
};
use lettre::message::{header::ContentType, Mailbox, MultiPart, SinglePart};

use crate::types::OutgoingMessage;

/// Core send logic, reusable by both the Tauri command and ImapProvider.
pub(crate) async fn send_message_impl(
    host: &str, port: u16, username: &str, password: &str, use_tls: bool,
    message: &OutgoingMessage,
) -> Result<String, String> {
    let from_mailbox: Mailbox = message.from.parse()
        .map_err(|e| format!("Invalid from address: {e}"))?;

    let mut email_builder = Message::builder()
        .from(from_mailbox)
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

    let email = email_builder
        .multipart(
            MultiPart::alternative()
                .singlepart(
                    SinglePart::builder()
                        .header(ContentType::TEXT_PLAIN)
                        .body(message.text.clone()),
                )
                .singlepart(
                    SinglePart::builder()
                        .header(ContentType::TEXT_HTML)
                        .body(message.html.clone()),
                ),
        )
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

/// Tauri command wrapper.
#[tauri::command]
pub async fn send_message(
    host: String, port: u16, username: String, password: String, use_tls: bool,
    message: OutgoingMessage,
) -> Result<String, String> {
    send_message_impl(&host, port, &username, &password, use_tls, &message).await
}
