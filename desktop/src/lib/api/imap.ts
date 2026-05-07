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

/** Download an attachment to the user's Downloads folder; returns absolute file path.
 *  NOTE: download_attachment stays on v1 — it does MIME parsing locally which
 *  the provider abstraction doesn't handle yet. */
export async function downloadAttachment(
  account: Account,
  folder: string,
  uid: number,
  index: number,
  filename: string,
): Promise<string> {
  return invoke<string>("download_attachment", {
    ...imapArgs(account),
    folder,
    uid,
    index,
    filename,
  });
}
