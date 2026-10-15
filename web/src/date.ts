// Use the user's calendar day, not the browser's day. formatToParts keeps the
// API's YYYY-MM-DD format stable across browser locale implementations.
export function localDateInTimezone(date: Date, timezone: string): string {
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: timezoneOrDefault(timezone), year: 'numeric', month: '2-digit', day: '2-digit',
  }).formatToParts(date)
  const value = (type: string) => parts.find(part => part.type === type)?.value
  return `${value('year')}-${value('month')}-${value('day')}`
}
// Empty historical profiles use the same default as server-side planning.
// Keep every non-empty value unchanged; the profile API validates IANA names.
export function timezoneOrDefault(timezone: string): string {
  return timezone.trim() ? timezone : 'Asia/Shanghai'
}
