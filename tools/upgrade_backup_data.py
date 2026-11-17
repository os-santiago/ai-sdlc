"""Bounded private PostgreSQL archives and retained-data restoration proofs.

Callers supply already identity-bound access to the original database or an
owned isolated restore database. This module grants no migration/rollback right.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import stat

from installer_process import capture

MAX_ARCHIVE=4*1024*1024
FILES={'database.dump','globals.sql'}
IDENTIFIER=re.compile('[a-z][a-z0-9_]{0,62}')



def digest(raw):return hashlib.sha256(raw).hexdigest()


def private_directory(path):
    info=path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid!=os.getuid() or info.st_mode&0o077:
        raise ValueError('Owned private backup directory required')


def write_archives(path,artifacts):
    if set(artifacts)!=FILES:raise ValueError('Complete database and globals archives required')
    private_directory(path)
    result={}
    for name in sorted(FILES):
        raw=artifacts[name]
        if not isinstance(raw,bytes) or not 1<=len(raw)<=MAX_ARCHIVE:
            raise ValueError('Bounded private archive required')
        fd=os.open(path/name,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
        with os.fdopen(fd,'wb') as stream:stream.write(raw);stream.flush();os.fsync(stream.fileno())
        result[name]={'bytes':len(raw),'sha256':digest(raw)}
    fd=os.open(path,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try:os.fsync(fd)
    finally:os.close(fd)
    return result


def verify_archives(path,expected):
    private_directory(path)
    if set(expected)!=FILES:raise ValueError('Original backup manifest required')
    for name in sorted(FILES):
        fd=os.open(path/name,os.O_RDONLY|os.O_NOFOLLOW)
        with os.fdopen(fd,'rb') as stream:
            info=os.fstat(stream.fileno())
            if (not stat.S_ISREG(info.st_mode) or info.st_uid!=os.getuid() or info.st_mode&0o077
                    or info.st_nlink!=1 or not 1<=info.st_size<=MAX_ARCHIVE):
                raise ValueError('Original private archive custody changed')
            raw=stream.read(MAX_ARCHIVE+1)
        if expected[name]!={'bytes':len(raw),'sha256':digest(raw)}:
            raise ValueError('Original backup archive changed')


class Database:
    def __init__(self,command,prefix,user='ai_sdlc'):
        if not IDENTIFIER.fullmatch(user):raise ValueError('Original database user required')
        self.command,self.prefix,self.user=command,list(prefix),user

    def sql(self,statement):
        raw=self.command(self.prefix+['psql','-X','-q','-A','-t','--no-password','-v','ON_ERROR_STOP=1',
                                      '-U',self.user,'-d','ai_sdlc','-c',statement],timeout=30)
        if not isinstance(raw,bytes) or len(raw)>MAX_ARCHIVE:raise ValueError('Backup observation exceeded bound')
        return raw.decode()

    def snapshot(self,*,exclude_restore_role=None):
        if exclude_restore_role is not None and not re.fullmatch('aisdlc_restore_[a-f0-9]{32}',exclude_restore_role):
            raise ValueError('Only the owned restore administrator may be excluded')
        # No payload or role password leaves the database: each row is hashed,
        # then the ordered hashes and row count bind its full multiset.
        identity=json.loads(self.sql("BEGIN READ ONLY;SET LOCAL row_security=off;SELECT json_build_object('database',current_database(),'superuser',(SELECT rolsuper FROM pg_roles WHERE rolname=current_user),'customTablespaces',(SELECT count(*) FROM pg_tablespace WHERE spcname NOT IN('pg_default','pg_global')));COMMIT;"))
        if identity!={'database':'ai_sdlc','superuser':True,'customTablespaces':0}:
            raise ValueError('Original bounded administrative database required')
        names=json.loads(self.sql("BEGIN READ ONLY;SELECT coalesce(json_agg(c.relname ORDER BY c.relname COLLATE \"C\"),'[]') FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind='r';COMMIT;"))
        if (not isinstance(names,list) or not 1<=len(names)<=512 or len(set(names))!=len(names)
                or any(not isinstance(name,str) or not IDENTIFIER.fullmatch(name) for name in names)):
            raise ValueError('Closed public table inventory required')
        def proof(name,query):
            return ("SELECT json_build_object('name','"+name+"','rows',count(*),'sha256',"
                    "encode(sha256(convert_to(coalesce(string_agg(h,'' ORDER BY h COLLATE \"C\"),''),'UTF8')),'hex')) "
                    "FROM (SELECT encode(sha256(convert_to(to_jsonb(t)::text,'UTF8')),'hex') h FROM ("+query+") t) d;")
        queries=[proof(name,'SELECT * FROM public."'+name+'"') for name in names]
        role_filter="rolname !~ '^pg_'"+(" AND rolname<>'"+exclude_restore_role+"'" if exclude_restore_role else '')
        queries.append(proof('@roles','SELECT to_jsonb(r)-\'oid\' AS role FROM pg_authid r WHERE '+role_filter))
        # initdb assigns built-in-to-built-in memberships to its own bootstrap
        # user. Preserve every edge/option, canonicalizing only that grantor.
        queries.append(proof('@membership',"SELECT a.rolname AS role,b.rolname AS member,CASE WHEN a.rolname ~ '^pg_' AND b.rolname ~ '^pg_' AND c.rolname=current_user THEN '@bootstrap' ELSE c.rolname END AS grantor,m.admin_option,m.inherit_option,m.set_option FROM pg_auth_members m JOIN pg_roles a ON a.oid=m.roleid JOIN pg_roles b ON b.oid=m.member JOIN pg_roles c ON c.oid=m.grantor"))
        queries.append(proof('@role-settings',"SELECT r.rolname,d.datname,s.setconfig FROM pg_db_role_setting s LEFT JOIN pg_roles r ON r.oid=s.setrole LEFT JOIN pg_database d ON d.oid=s.setdatabase"))
        raw=self.sql("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;SET LOCAL row_security=off;SET LOCAL statement_timeout='10000ms';"+''.join(queries)+'COMMIT;')
        result={}
        for line in raw.splitlines():
            value=json.loads(line)
            if (not isinstance(value,dict) or set(value)!={'name','rows','sha256'} or value['name'] in result
                    or type(value['rows']) is not int or value['rows']<0 or not re.fullmatch('[a-f0-9]{64}',value['sha256'])):
                raise ValueError('Malformed retained database observation')
            result[value['name']]={key:value[key] for key in ('rows','sha256')}
        if set(result)!=set(names)|{'@roles','@membership','@role-settings'}:
            raise ValueError('Incomplete retained database observation')
        # This deterministic-key dump is compared only, never saved or executed.
        # Keep ownership, ACLs, policies, functions and database settings intact.
        schema=self.command(self.prefix+['pg_dump','--schema-only','--create','--no-password',
            '--restrict-key=aiSdlcSchemaComparisonOnly','--lock-wait-timeout=5000',
            '--username='+self.user,'--dbname=ai_sdlc'],timeout=30)
        if not isinstance(schema,bytes) or not 1<=len(schema)<=MAX_ARCHIVE:
            raise ValueError('Original schema comparison exceeded bound')
        result['@schema']={'bytes':len(schema),'sha256':digest(schema)}
        return result

    def export(self):
        before=self.snapshot()
        archive=self.command(self.prefix+['pg_dump','--format=custom','--create','--no-password',
            '--lock-wait-timeout=5000','--username='+self.user,'--dbname=ai_sdlc'],timeout=30)
        globals_sql=self.command(self.prefix+['pg_dumpall','--globals-only','--no-password',
            '--username='+self.user,'--database=ai_sdlc'],timeout=30)
        if not archive.startswith(b'PGDMP') or not globals_sql.strip():
            raise ValueError('Incomplete PostgreSQL backup')
        if self.snapshot()!=before:raise ValueError('Original retained database changed during backup')
        return {'database.dump':archive,'globals.sql':globals_sql},before
