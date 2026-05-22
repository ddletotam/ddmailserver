import { invoke } from "@tauri-apps/api/core";
import type { Account } from "../types/mail";

function imapArgs(account: Account) {
  return {
    host: account.imap_host,
    port: account.imap_port,
    username: account.username,
    password: account.password,
    useTls: account.use_tls,
  };
}

/** Fetch raw RFC-822 source of a single message */
export async function fetchMessageSource(
  account: Account,
  folder: string,
  uid: number,
): Promise<string> {
  return invoke<string>("v2_fetch_message_source", {
    accountId: account.id,
    folder,
    uid,
  });
}

/** Save an attachment to an explicit path (from a save-file dialog); returns final path. */
export async function saveAttachmentToPath(
  account: Account,
  folder: string,
  uid: number,
  index: number,
  savePath: string,
): Promise<string> {
  return invoke<string>("v2_save_attachment_to_path", {
    accountId: account.id,
    folder,
    uid,
    index,
    savePath,
  });
}

/** Download an attachment to the user's Downloads folder; returns absolute file path.
 *  Routes through the provider abstraction: native accounts hit the
 *  /messages/{id}/attachments/{index} HTTP endpoint, IMAP accounts pull the
 *  raw message and extract via mailparse. */
export async function downloadAttachment(
  account: Account,
  folder: string,
  uid: number,
  index: number,
  filename: string,
): Promise<string> {
  return invoke<string>("v2_download_attachment", {
    accountId: account.id,
    folder,
    uid,
    index,
    filename,
  });
}
