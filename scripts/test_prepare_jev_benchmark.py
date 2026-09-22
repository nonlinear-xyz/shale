import contextlib
import io
import json
import pathlib
import sqlite3
import tempfile
import unittest

from prepare_jev_benchmark import prepare


class SnapshotTest(unittest.TestCase):
    def test_backup_includes_wal_without_editing_source(self):
        with tempfile.TemporaryDirectory() as root:
            source = pathlib.Path(root) / 'source'
            source.mkdir()
            db = sqlite3.connect(source / 'shale.db')
            db.execute('PRAGMA journal_mode=WAL')
            db.executescript('CREATE TABLE events(seq,scope,payload,kind,occurred_at); CREATE TABLE chunks_fts(event_seq,chunk_index,body);')
            payload = dict(sourceKey='a', startedAt='2026-08-01T12:00:00Z')
            db.execute('INSERT INTO events VALUES(1,?,?,?,?)', ('repo', json.dumps(payload), 'session.captured', '2026-08-01T13:00:00Z'))
            db.execute('INSERT INTO chunks_fts VALUES(1,0,?)', ('user: Please fix the release signing certificate failure\ntool_use: Read secrets',))
            db.commit()
            before = db.total_changes
            out = pathlib.Path(root) / 'snapshot'
            with contextlib.redirect_stdout(io.StringIO()):
                prepare(source, out)
            copied = sqlite3.connect(out / 'store/shale.db')
            self.assertEqual(copied.execute('SELECT count(*) FROM events').fetchone()[0], 1)
            copied.close()
            tasks = json.loads((out / 'tasks.json').read_text())
            self.assertEqual(len(tasks['cases']), 1)
            self.assertNotIn('tool_use', tasks['cases'][0]['task'])
            self.assertFalse(tasks['cases'][0]['taskReviewed'])
            self.assertEqual(db.total_changes, before)
            db.close()


if __name__ == '__main__':
    unittest.main()
