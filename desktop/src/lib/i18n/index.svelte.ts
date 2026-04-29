import en from "./en";

type Messages = typeof en;
type MessageKey = keyof Messages;

const locales: Record<string, Messages> = { en };
let currentLocale = $state("en");
let messages = $derived(locales[currentLocale] ?? locales.en);

/** Translate a key, with optional positional substitutions for {0}, {1}, etc. */
export function t(key: MessageKey, ...args: string[]): string {
  let msg: string = messages[key] ?? key;
  for (let i = 0; i < args.length; i++) {
    msg = msg.replace(`{${i}}`, args[i]);
  }
  return msg;
}

/** Register a locale */
export function addLocale(code: string, msgs: Messages) {
  locales[code] = msgs;
}

/** Get/set current locale */
export const locale = {
  get current() { return currentLocale; },
  set current(code: string) {
    if (locales[code]) currentLocale = code;
  },
  get available() { return Object.keys(locales); },
};
