import hashlib,json,tempfile,time,unittest
from pathlib import Path
from unittest.mock import patch
import learning_tick as tick

class FakeCLI:
    def __init__(self, fail=False, allow=True):
        self.deadline=time.monotonic()+300;self.calls=[];self.fail=fail;self.allow=allow;self.intake=None
    def call(self,*a):
        self.calls.append(a)
        if a[:2]==('source','observe'):
            if self.fail: raise tick.TickError('intake unavailable')
            self.intake=json.loads(Path(a[2]).read_text())
            return '- status: ok\n- candidate: src_cand_fixture\n- no_promote: np_fixture\n'
        return 'ok'
    def json(self,*a):
        self.calls.append(a)
        if a[:2]==('work','act'):
            i=json.loads(Path(a[-1]).read_text())
            return {'id':'pol_fixture','action_intent_id':i['id'],'outcome':'allow' if self.allow else 'hold'}
        if a[:2]==('finding','record'): return {'id':'find_0123456789ab'}
        raise AssertionError(a)

class Tests(unittest.TestCase):
    def fixture(self,out):
        (out/'pending').mkdir();(out/'content'/'sha256').mkdir(parents=True)
        raw=b'untrusted source text';h=hashlib.sha256(raw).hexdigest();body=out/'content'/'sha256'/(h+'.txt');body.write_bytes(raw)
        obs={'schema_version':tick.scout.OBS_SCHEMA,'id':'srcobs_fixture','source_ref':{'path':'https://github.example/example/repo'},'maps_to':[{'promote_allowed':False}],'evidence_refs':['pending_body:sha256:'+h], 'scout_context':{'frontier':'fixture'}}
        f=out/'srcobs_fixture.json';f.write_text(json.dumps(obs));p=out/'pending'/'srcobs_fixture.json'
        p.write_text(json.dumps({'observation_id':'srcobs_fixture','source_observation_file':str(f),'body_files':{'README.md':str(body)}}));return p,body
    def test_failed_intake_keeps_pending_without_ack(self):
        with tempfile.TemporaryDirectory() as d:
            out=Path(d);p,_=self.fixture(out);cli=FakeCLI(fail=True)
            with patch.object(tick.scout,'run') as scout: r=tick.tick(cli,{},out)
            scout.assert_not_called();self.assertTrue(p.exists());self.assertTrue(r['failures']);self.assertTrue(any(a[:2]==('work','close') and 'failed' in a for a in cli.calls))
    def test_custody_then_intake_then_ack(self):
        with tempfile.TemporaryDirectory() as d:
            out=Path(d);self.fixture(out);cli=FakeCLI()
            with patch.object(tick.scout,'run') as scout: r=tick.tick(cli,{},out)
            self.assertFalse(r['failures']);scout.assert_called_once_with({},str(out),ack='srcobs_fixture')
            self.assertFalse(any(x.startswith('pending_body:') for x in cli.intake['evidence_refs']));self.assertIn('finding:find_0123456789ab',cli.intake['evidence_refs']);self.assertNotIn('scout_context',cli.intake)
    def test_policy_hold_prevents_fetch_and_intake(self):
        with tempfile.TemporaryDirectory() as d:
            out=Path(d);self.fixture(out);cli=FakeCLI(allow=False)
            with patch.object(tick.scout,'run') as scout: r=tick.tick(cli,{},out)
            scout.assert_not_called();self.assertTrue(r['failures']);self.assertFalse(any(a[:2] in [('source','observe'),('finding','record')] for a in cli.calls))
    def test_tampered_body_refused_before_custody(self):
        with tempfile.TemporaryDirectory() as d:
            out=Path(d);p,body=self.fixture(out);body.write_text('tampered');cli=FakeCLI()
            with self.assertRaisesRegex(tick.TickError,'hash mismatch'): tick.intake(cli,'work_fixture',out,p)
            self.assertFalse(cli.calls)
    def test_dry_run_writes_nothing(self):
        with tempfile.TemporaryDirectory() as d:
            out=Path(d)/'absent';cfg=Path(d)/'cfg.json';cfg.write_text('{}')
            with patch.object(tick.scout,'validate_config',return_value=[{}]),patch.object(tick,'CLI') as cli:
                self.assertEqual(tick.main(['--config',str(cfg),'--output-dir',str(out),'--dry-run']),0)
            self.assertFalse(out.exists());cli.assert_not_called()
if __name__=='__main__': unittest.main()
