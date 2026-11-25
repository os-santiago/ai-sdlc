# Scheduled PostgreSQL backup and operator-started restore
# (docs/operations/backup-and-restore.md). POSIX sh: it runs under the
# PostgreSQL image's busybox shell.
#
# Inputs (environment):
#   BACKUP_MODE                "backup" (the CronJob) or "restore" (an
#                              operator's restore Job). Required: a restore is
#                              never inferred from another variable.
#   DATABASE_URL               the bootstrap owner's postgresql:// URL, from a Secret
#   BACKUP_DIR                 the backup volume
#   BACKUP_RETENTION           how many complete backups to keep (at least 1)
#   BACKUP_LOCK_STALE_SECONDS  this run's deadline (the Job's
#                              activeDeadlineSeconds), recorded in the lock
#   RESTORE_FILE               with BACKUP_MODE=restore only: the backup file
#                              name to restore
#
# The password never reaches a command line or the log. It is split from the
# URL, percent-decoded into PGPASSWORD, and the PostgreSQL tools receive the
# URL without it, rebuilt with only allowlisted query parameters. Any URL the
# script cannot split safely is refused without being echoed.
set -eu
# Owner read-write, group read: a pod running with the volume's fsGroup can
# read a backup (for example the inspect pod in the runbook), nobody else can.
umask 027

fail() {
  echo "backup: $*" >&2
  exit 1
}

mode="${BACKUP_MODE:-}"
case "$mode" in
  backup)
    [ -z "${RESTORE_FILE:-}" ] || fail "RESTORE_FILE is set but BACKUP_MODE is backup; set BACKUP_MODE=restore to restore"
    ;;
  restore)
    [ -n "${RESTORE_FILE:-}" ] || fail "BACKUP_MODE=restore needs RESTORE_FILE"
    case "$RESTORE_FILE" in
      ai-sdlc-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z.dump) ;;
      *) fail "RESTORE_FILE must name a backup file" ;;
    esac
    ;;
  *) fail "BACKUP_MODE must be backup or restore" ;;
esac

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

# Percent-decode $1 (RFC 3986) into $decoded. $2 names the part in errors,
# which never include the input.
decode() {
  undecoded="$1"
  decoded=""
  while [ -n "$undecoded" ]; do
    case "$undecoded" in
      %[0-9A-Fa-f][0-9A-Fa-f]*)
        hex="${undecoded#%}"
        hex="${hex%"${hex#??}"}"
        undecoded="${undecoded#%??}"
        [ "$hex" != 00 ] || fail "DATABASE_URL has an encoded NUL in its $2"
        char="$(printf "\\$(printf '%03o' "0x$hex")x")"
        decoded="$decoded${char%x}"
        ;;
      %*) fail "DATABASE_URL has an invalid percent escape in its $2" ;;
      *)
        rest_="${undecoded#?}"
        decoded="$decoded${undecoded%"$rest_"}"
        undecoded="$rest_"
        ;;
    esac
  done
  unset undecoded hex char rest_
}

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

# Host, port and database, then the query. libpq decodes parameter names, so
# a name is decoded before it is checked, and only allowlisted, non-secret
# parameters are passed on. Everything else, password included, is refused.
target="$hostport$path"
location="${target%%\?*}"
query="${target#"$location"}"
query="${query#\?}"
allowed=" sslmode sslrootcert sslcrl sslsni connect_timeout application_name target_session_attrs gssencmode channel_binding keepalives keepalives_idle keepalives_interval keepalives_count "
kept=""
set -f
IFS='&'
for parameter in $query; do
  unset IFS
  [ -n "$parameter" ] || continue
  case "$parameter" in
    *=*) ;;
    *) fail "DATABASE_URL has a query parameter without a value" ;;
  esac
  decode "${parameter%%=*}" "query parameter name"
  key="$(printf '%s' "$decoded" | tr 'A-Z' 'a-z')"
  case "$key" in
    password) fail "DATABASE_URL must not carry a password query parameter" ;;
    *[!a-z_]* | '') fail "DATABASE_URL has an unsupported query parameter" ;;
  esac
  case "$allowed" in
    *" $key "*) ;;
    *) fail "DATABASE_URL has an unsupported query parameter; allowed:$allowed" ;;
  esac
  kept="$kept${kept:+&}$key=${parameter#*=}"
  IFS='&'
done
unset IFS
set +f

decode "$encoded" "password"
password="$decoded"
unset DATABASE_URL uri rest authority userinfo encoded decoded target query parameter key
if [ -n "$password" ]; then
  PGPASSWORD="$password"
  export PGPASSWORD
fi
unset password
conninfo="$scheme://${user:+$user@}$location${kept:+?$kept}"

# Reads every byte of an archive: the table of contents and every data block.
# pg_restore --list alone reads only the table of contents at the start.
archive_complete() {
  pg_restore --file=/dev/null "$1"
}

# --- One backup or restore at a time on this volume. ---
# concurrencyPolicy: Forbid does not cover a Job created by hand from the
# CronJob. The owner records its own deadline (start + its Job's
# activeDeadlineSeconds) in the lock. No run outlives its deadline, so a lock
# past its deadline plus a margin was left by a killed run. A lock without a
# readable deadline (written by an older script, or a run killed right after
# mkdir) is reclaimed only after a conservative day.
lock="$dir/.ai-sdlc-backup.lock"
margin=300
legacy=86400
now="$(date +%s)"
if ! mkdir "$lock" 2> /dev/null; then
  deadline="$(cat "$lock/deadline" 2> /dev/null || true)"
  case "$deadline" in
    '' | *[!0-9]*)
      since="$(stat -c %Y "$lock" 2> /dev/null || echo "$now")"
      deadline=$((since + legacy - margin))
      ;;
  esac
  if [ "$now" -gt $((deadline + margin)) ]; then
    echo "backup: removing a lock whose owner's deadline has passed"
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
printf '%s\n' "$((now + stale))" > "$lock/deadline.tmp"
mv -- "$lock/deadline.tmp" "$lock/deadline"

if [ "$mode" = restore ]; then
  # Restore: replace the database's objects and rows with the archive's, in
  # one transaction. Refuse while any runtime component is still connected.
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
