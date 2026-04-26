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
  return invoke<string>("fetch_message_source", {
    ...imapArgs(account),
    folder,
    uid,
  });
}
