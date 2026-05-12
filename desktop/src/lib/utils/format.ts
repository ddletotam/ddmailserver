/** Format unix timestamp to HH:MM */
export function formatTime(ts: number): string {
  if (!ts) return "";
  return new Date(ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

/** Format unix timestamp to relative date string */
export function formatDate(ts: number): string {
  if (!ts) return "";
  const date = new Date(ts * 1000);
  const now = new Date();
  const diff = now.getTime() - date.getTime();
  const dayMs = 86400000;
  if (diff < dayMs && date.getDate() === now.getDate())
    return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (diff < 7 * dayMs)
    return date.toLocaleDateString([], { weekday: "short" });
  return date.toLocaleDateString([], { day: "numeric", month: "short" });
}

/** Format full date for date separators */
export function formatDateSeparator(ts: number): string {
  const date = new Date(ts * 1000);
  const now = new Date();
  const diff = now.getTime() - date.getTime();
  if (diff < 86400000 && date.getDate() === now.getDate()) return "Today";
  if (diff < 172800000) return "Yesterday";
  return date.toLocaleDateString([], { day: "numeric", month: "long", year: "numeric" });
}

/** Format unix timestamp to short date (day + month) */
export function formatDateShort(ts: number): string {
  if (!ts) return "";
  return new Date(ts * 1000).toLocaleDateString([], { day: "numeric", month: "short" });
}

/** Format unix timestamp to short date + time: "24 Apr, 14:30" */
export function formatDateTime(ts: number): string {
  if (!ts) return "";
  const d = new Date(ts * 1000);
  const date = d.toLocaleDateString([], { day: "numeric", month: "short" });
  const time = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return `${date}, ${time}`;
}

/** Format byte size to human-readable string */
export function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** Stable hash-based color from a string (for avatars, sender names) */
export function hashColor(str: string): string {
  const colors = [
    "#e17076", "#7bc862", "#e5ca77", "#65aadd",
    "#a695e7", "#ee7aae", "#6ec9cb", "#faa774",
  ];
  let hash = 0;
  for (let i = 0; i < str.length; i++)
    hash = ((hash << 5) - hash + str.charCodeAt(i)) | 0;
  return colors[Math.abs(hash) % colors.length];
}

/** Extract initials from a name or email */
export function initials(label: string): string {
  const parts = label.split(/[\s@]+/).filter(Boolean);
  if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
  return label.substring(0, 2).toUpperCase();
}

/** Strip angle-bracket email from display name: "Name <email>" → "Name" */
export function cleanName(raw: string): string {
  const stripped = raw.replace(/<[^>]*>/g, "").trim();
  return stripped || raw;
}

/** Check if two timestamps fall on the same calendar day */
export function sameDay(ts1: number, ts2: number): boolean {
  const d1 = new Date(ts1 * 1000);
  const d2 = new Date(ts2 * 1000);
  return d1.getFullYear() === d2.getFullYear() && d1.getMonth() === d2.getMonth() && d1.getDate() === d2.getDate();
}

/** Build gravatar URL from hash, or null */
export function gravatarUrl(hash: string | null): string | null {
  return hash ? `https://www.gravatar.com/avatar/${hash}?d=404&s=96` : null;
}

/** Format an RFC 5322 mailbox: `Display Name <email>` (quoted when the name
 *  contains specials). Returns just the email when no name is provided. */
export function formatFromHeader(name: string | undefined | null, email: string): string {
  const trimmed = (name ?? "").trim();
  if (!trimmed) return email;
  const needsQuoting = /[",;:<>@\[\]\\()]/.test(trimmed);
  const escaped = trimmed.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
  const display = needsQuoting ? `"${escaped}"` : trimmed;
  return `${display} <${email}>`;
}
