# Scheduled PostgreSQL backup and operator-started restore
# (docs/operations/backup-and-restore.md). POSIX sh: it runs under the
# PostgreSQL image's busybox shell.
#
# Inputs (environment):
#   DATABASE_URL               the bootstrap owner's postgresql:// URL, from a Secret
#   BACKUP_DIR                 the backup volume
#   BACKUP_RETENTION           how many complete backups to keep (at least 1)
#   BACKUP_LOCK_STALE_SECONDS  age after which a volume lock is abandoned
#                              (the Job's activeDeadlineSeconds)
#   RESTORE_FILE               set only by an operator's restore Job: the backup
#                              file name to restore instead of taking a backup
#
# The password never reaches a command line or the log. It is split from the
# URL, percent-decoded into PGPASSWORD, and the PostgreSQL tools receive the
# URL without it. Any URL the script cannot split safely is refused without
# being echoed.
set -eu
# Owner read-write, group read: a pod running with the volume's fsGroup can
# read a backup (for example the inspect pod in the runbook), nobody else can.
umask 027

fail() {
  echo "backup: $*" >&2
  exit 1
}

dir="${BACKUP_DIR:?BACKUP_DIR is required}"
retention="${BACKUP_RETENTION:?BACKUP_RETENTION is required}"
stale="${BACKUP_LOCK_STALE_SECONDS:-3600}"
case "$retention" in
  '' | *[!0-9]* | 0*) fail "BACKUP_RETENTION must be a positive integer" ;;
esac
case "$stale" in
  '' | *[!0-9]* | 0*) fail "BACKUP_LOCK_STALE_SECONDS must be a positive integer" ;;
esac
[ -d "$dir" ] || fail "backup directory is missing"
[ -w "$dir" ] || fail "backup directory is not writable"

# --- Split the URL. Messages below never include any part of it. ---
uri="${DATABASE_URL:?DATABASE_URL is required}"
case "$uri" in
  postgresql://* | postgres://*) ;;
  *) fail "DATABASE_URL must be a postgresql:// URL" ;;
esac
scheme="${uri%%://*}"
rest="${uri#*://}"
authority="${rest%%/*}"
path="${rest#"$authority"}"
# An "@" after the authority means the user name or password holds a raw "/":
# the split would leave the password in the URL. Refuse instead.
case "$path" in
  *@*) fail "DATABASE_URL has a raw '/' in its user name or password; percent-encode it as %2F" ;;
esac
case "$authority" in
  *@*)
    userinfo="${authority%@*}"
    hostport="${authority##*@}"
    ;;
  *)
    userinfo=""
    hostport="$authority"
    ;;
esac
case "$userinfo" in
  *:*)
    user="${userinfo%%:*}"
    encoded="${userinfo#*:}"
    ;;
  *)
    user="$userinfo"
    encoded=""
    ;;
esac
[ -n "$hostport" ] || fail "DATABASE_URL has no host"
case "$hostport" in
  [/?#]*) fail "DATABASE_URL has no host" ;;
esac
# Everything after the host goes to the tools as it is: it must hold no password.
case "$(printf '%s' "$hostport$path" | tr 'A-Z' 'a-z')" in
  *'?password='* | *'&password='*) fail "DATABASE_URL must not carry a password query parameter" ;;
esac

# Percent-decode the password (RFC 3986). Raw characters other than "/" are
# accepted as they are.
password=""
while [ -n "$encoded" ]; do
  case "$encoded" in
    %[0-9A-Fa-f][0-9A-Fa-f]*)
      hex="${encoded#%}"
      hex="${hex%"${hex#??}"}"
      encoded="${encoded#%??}"
      [ "$hex" != 00 ] || fail "DATABASE_URL has an encoded NUL in its password"
      char="$(printf "\\$(printf '%03o' "0x$hex")x")"
      password="$password${char%x}"
      ;;
    %*) fail "DATABASE_URL has an invalid percent escape in its password" ;;
    *)
      tail="${encoded#?}"
      password="$password${encoded%"$tail"}"
      encoded="$tail"
      ;;
  esac
done
unset DATABASE_URL uri rest authority userinfo encoded hex char tail
if [ -n "$password" ]; then
  PGPASSWORD="$password"
  export PGPASSWORD
fi
unset password
if [ -n "$user" ]; then
  conninfo="$scheme://$user@$hostport$path"
else
  conninfo="$scheme://$hostport$path"
fi

# Reads every byte of an archive: the table of contents and every data block.
# pg_restore --list alone reads only the table of contents at the start.
archive_complete() {
  pg_restore --file=/dev/null "$1"
}

# --- One backup or restore at a time on this volume. ---
# concurrencyPolicy: Forbid does not cover a Job created by hand from the
# CronJob. No run outlives the Job's activeDeadlineSeconds, so an older lock
# was left by a killed run.
lock="$dir/.ai-sdlc-backup.lock"
if ! mkdir "$lock" 2> /dev/null; then
  now="$(date +%s)"
  since="$(stat -c %Y "$lock" 2> /dev/null || echo "$now")"
  if [ $((now - since)) -gt "$stale" ]; then
    echo "backup: removing a lock older than $stale seconds"
    rm -rf -- "$lock"
    mkdir "$lock" 2> /dev/null || fail "another backup or restore holds the volume lock"
  else
    fail "another backup or restore holds the volume lock"
  fi
fi
trap 'rm -rf -- "$lock"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
date -u +%Y-%m-%dT%H:%M:%SZ > "$lock/started"

if [ -n "${RESTORE_FILE:-}" ]; then
  # Restore: replace the database's objects and rows with the archive's, in
  # one transaction. Refuse while any runtime component is still connected.
  case "$RESTORE_FILE" in
    ai-sdlc-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z.dump) ;;
    *) fail "RESTORE_FILE must name a backup file" ;;
  esac
  [ -f "$dir/$RESTORE_FILE" ] || fail "$RESTORE_FILE is not on the backup volume"
  if [ -f "$dir/$RESTORE_FILE.sha256" ]; then
    (cd "$dir" && sha256sum -c "$RESTORE_FILE.sha256" > /dev/null) || fail "$RESTORE_FILE does not match its checksum"
  fi
  archive_complete "$dir/$RESTORE_FILE" || fail "$RESTORE_FILE is incomplete or unreadable"
  sessions="$(psql --no-password --no-psqlrc --tuples-only --no-align --dbname="$conninfo" \
    --command="SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND usename LIKE 'aisdlc\_%' AND pid <> pg_backend_pid()")" ||
    fail "cannot connect to the database"
  [ "$sessions" = 0 ] || fail "$sessions runtime sessions are connected; scale the components to zero first"
  echo "backup: restoring $RESTORE_FILE"
  pg_restore --clean --if-exists --no-owner --single-transaction --exit-on-error --no-password \
    --dbname="$conninfo" "$dir/$RESTORE_FILE" || fail "pg_restore failed; the transaction was rolled back"
  echo "backup: restored $RESTORE_FILE"
  exit 0
fi

# Partial files can only be left by a killed run: this run holds the lock.
for partial in "$dir"/.ai-sdlc-*.partial; do
  [ -e "$partial" ] || continue
  rm -f -- "$partial"
done

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
name="ai-sdlc-$stamp.dump"
target="$dir/$name"
temporary="$dir/.$name.partial"
checksum="$dir/.$name.sha256.partial"
[ ! -e "$target" ] || fail "$name already exists"

echo "backup: dumping to $name"
if ! pg_dump --format=custom --no-password --file="$temporary" --dbname="$conninfo"; then
  rm -f -- "$temporary"
  fail "pg_dump failed; no backup was written"
fi
if ! archive_complete "$temporary"; then
  rm -f -- "$temporary"
  fail "the new archive is incomplete or unreadable; no backup was written"
fi
hash="$(sha256sum < "$temporary")"
printf '%s  %s\n' "${hash%% *}" "$name" > "$checksum"
sync "$temporary" "$checksum" 2> /dev/null || sync
mv -- "$temporary" "$target"
mv -- "$checksum" "$target.sha256"
echo "backup: wrote $name ($(wc -c < "$target" | tr -d ' ') bytes)"

# Keep this run's backup and the newest BACKUP_RETENTION - 1 others. Names
# sort by time; this run's file is never pruned, even if the clock went back.
ls -1 "$dir" | grep -E '^ai-sdlc-[0-9]{8}T[0-9]{6}Z\.dump$' | grep -vxF "$name" | sort -r | tail -n "+$retention" |
  while IFS= read -r old; do
    rm -f -- "$dir/$old" "$dir/$old.sha256"
    echo "backup: pruned $old"
  done
echo "backup: done"
