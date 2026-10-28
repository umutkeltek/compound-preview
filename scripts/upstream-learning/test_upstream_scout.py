import json, os, tempfile, unittest, subprocess
from unittest.mock import patch
from pathlib import Path
import sys
sys.path.insert(0, str(Path(__file__).parent))
import upstream_scout as s

class Fake:
    def __init__(self, sha="a"*40, fail=False, body=b"bounded evidence text"):
        self.sha, self.fail, self.body, self.calls = sha, fail, body, []
    def get(self, url, limit, timeout):
        self.calls.append(url)
        if self.fail: raise s.ScoutError("synthetic unavailable")
        if "/commits/" in url: return json.dumps({"sha": self.sha}).encode()
        if "/contents/" in url: return json.dumps({"type":"file", "path":"README.md"}).encode()
        if len(self.body) > limit: raise s.ScoutError("response exceeds byte bound")
        return self.body

def cfg():
    return {"schema_version":s.CONFIG_SCHEMA,"sources":[{"repo_url":f"https://{s.GITHUB_WEB_HOST}/example/repo","branch":"main","existing_compound_owner":"owned-capability","frontier":"bounded integration frontier","inspection_paths":["README.md"],"success_criterion":"fixture proves improvement","falsifier":"duplicate or unbounded","cadence":"weekly"}]}

class ScoutTests(unittest.TestCase):
    def setUp(self):
        hosts=patch.multiple(s,GITHUB_WEB_HOST="github.example",GITHUB_API_HOST="api.github.example",GITHUB_RAW_HOST="raw.github.example")
        hosts.start();self.addCleanup(hosts.stop)

    def test_actual_source_observation_shape_and_custody(self):
        with tempfile.TemporaryDirectory() as d:
            r=s.run(cfg(),d,s.Fetch(Fake().get),now="2026-09-22T00:00:00Z")
            with open(Path(d)/(r["staged"][0]+".json")) as f: obs=json.load(f)
            self.assertEqual(obs["schema_version"],"compound.source_observation.v0.1"); self.assertEqual(obs["source_ref"]["source_class"],"evidence")
            self.assertEqual(obs["maps_to"][0]["candidate_kind"],"capability_observation"); self.assertFalse(obs["maps_to"][0]["promote_allowed"])
            with open(Path(d)/"pending"/(r["staged"][0]+".json")) as pf: pending=json.load(pf)
            self.assertTrue(Path(pending["raw_body_file"]).is_file()); self.assertEqual(pending["source_observation_file"],str(Path(d)/(r["staged"][0]+".json")))

    def test_unchanged_pending_is_available_without_duplicate(self):
        with tempfile.TemporaryDirectory() as d:
            f=Fake(); first=s.run(cfg(),d,s.Fetch(f.get)); second=s.run(cfg(),d,s.Fetch(f.get))
            self.assertEqual(second["staged"],[]); self.assertEqual(second["pending"],first["pending"]); self.assertEqual(len(list(Path(d).glob("srcobs_*.json"))),1)

    def test_ack_then_same_source_is_silent(self):
        with tempfile.TemporaryDirectory() as d:
            first=s.run(cfg(),d,s.Fetch(Fake().get)); self.assertEqual(s.run(cfg(),d,ack=first["staged"][0])["status"],"acked")
            second=s.run(cfg(),d,s.Fetch(Fake().get)); self.assertEqual(second["staged"],[]); self.assertEqual(second["pending"],[])

    def test_changed_source_keeps_old_and_invalidates(self):
        with tempfile.TemporaryDirectory() as d:
            s.run(cfg(),d,s.Fetch(Fake("a"*40, body=b"old evidence").get)); r=s.run(cfg(),d,s.Fetch(Fake("b"*40, body=b"new evidence").get)); self.assertEqual(len(r["staged"]),1)
            with open(Path(d)/(r["staged"][0]+".json")) as f: obs=json.load(f)
            self.assertIn("invalidates_observation_id",obs); self.assertEqual(len(list(Path(d).glob("srcobs_*.json"))),2)

    def test_head_change_with_same_bodies_is_quiet(self):
        with tempfile.TemporaryDirectory() as d:
            s.run(cfg(),d,s.Fetch(Fake("a"*40).get)); r=s.run(cfg(),d,s.Fetch(Fake("b"*40).get))
            self.assertEqual(r["staged"],[]); self.assertEqual(len(list(Path(d).glob("srcobs_*.json"))),1); self.assertEqual(len(list((Path(d)/"pending").glob("*.json"))),1)

    def test_failure_after_previous_stage_preserves_pending_and_retries(self):
        with tempfile.TemporaryDirectory() as d:
            c=cfg(); c["sources"].append(dict(c["sources"][0],repo_url=f"https://{s.GITHUB_WEB_HOST}/example/two")); f=Fake(); f2=Fake(fail=True)
            first=s.run(c,d,s.Fetch(f.get)); self.assertEqual(len(first["staged"]),2)
            # Existing state remains intact when the next source fails.
            second=s.run(c,d,s.Fetch(f2.get)); self.assertEqual(len(second["failures"]),2)
            with open(Path(d)/".state.json") as sf: self.assertEqual(len(json.load(sf)["observations"]),2)

    def test_rejects_home_and_symlink_output(self):
        with tempfile.TemporaryDirectory() as d:
            os.environ["COMPOUND_HOME"]=d
            with self.assertRaises(s.ScoutError): s.run(cfg(),os.path.join(d,"stage"),s.Fetch(Fake().get))
            del os.environ["COMPOUND_HOME"]
            link=os.path.join(d,"link"); os.symlink(os.path.join(d,"real"),link)
            with self.assertRaises(s.ScoutError): s.run(cfg(),link,s.Fetch(Fake().get))

    def test_rejects_remote_symlink_and_traversal(self):
        with tempfile.TemporaryDirectory() as d:
            class Link(Fake):
                def get(self,url,limit,timeout):
                    if "/contents/" in url: return json.dumps({"type":"symlink","path":"README.md"}).encode()
                    return super().get(url,limit,timeout)
            self.assertEqual(len(s.run(cfg(),d,s.Fetch(Link().get))["failures"]),1)
            bad=cfg(); bad["sources"][0]["inspection_paths"]=["../secret"]
            with self.assertRaises(s.ScoutError): s.validate_config(bad)

    def test_total_bound_and_no_content_scoring(self):
        with tempfile.TemporaryDirectory() as d:
            c=cfg(); c["sources"][0]["inspection_paths"]=["README.md"]
            r=s.run(c,d,s.Fetch(Fake(body=b"x"*300000).get)); self.assertEqual(len(r["failures"]),1)
            self.assertFalse(any("priority" in x for x in r.get("candidates",[])))

    def test_metadata_counts_against_whole_run_budget(self):
        with tempfile.TemporaryDirectory() as d:
            f=Fake()
            with patch.object(s, 'MAX_BYTES', 60):
                r=s.run(cfg(),d,s.Fetch(f.get))
            self.assertTrue(r['failures']); self.assertFalse(r['staged'])
            self.assertEqual(len(f.calls),2)
            self.assertIn('whole-run',r['failures'][0]['error'])

    def test_deadline_checked_before_next_request(self):
        with tempfile.TemporaryDirectory() as d:
            clock=[0.0]; f=Fake()
            def get(url,limit,timeout):
                result=f.get(url,limit,timeout); clock[0]=s.MAX_WALL+1; return result
            with patch.object(s.time,'monotonic',side_effect=lambda:clock[0]):
                r=s.run(cfg(),d,s.Fetch(get))
            self.assertEqual(len(f.calls),1); self.assertTrue(r['failures'])

    def test_corrupt_state_never_resets_history_or_fetches(self):
        with tempfile.TemporaryDirectory() as d:
            state=Path(d)/'.state.json';state.write_text('{bad');f=Fake()
            with self.assertRaises(json.JSONDecodeError): s.run(cfg(),d,s.Fetch(f.get))
            self.assertEqual(state.read_text(),'{bad');self.assertEqual(f.calls,[])

    def test_ack_persists_before_pending_cleanup(self):
        with tempfile.TemporaryDirectory() as d:
            oid=s.run(cfg(),d,s.Fetch(Fake().get))['staged'][0]
            with patch.object(s.os,'unlink',side_effect=OSError('interrupted cleanup')):
                with self.assertRaises(OSError): s.run(cfg(),d,s.Fetch(Fake().get),ack=oid)
            state=json.loads((Path(d)/'.state.json').read_text())
            self.assertEqual(state['observations'][oid]['status'],'acked')
            s.run(cfg(),d,s.Fetch(Fake().get),ack=oid)
            self.assertFalse((Path(d)/'pending'/(oid+'.json')).exists())

    def test_live_source_observe_accepts_staged_record_in_temp_home(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d); stage=root/"stage"; home=root/"home"; r=s.run(cfg(),str(stage),s.Fetch(Fake().get),now="2026-09-22T00:00:00Z")
            obs=stage/(r["staged"][0]+".json"); env=dict(os.environ,COMPOUND_HOME=str(home))
            repo=Path(__file__).resolve().parents[2]
            init=subprocess.run(["go","run","./cmd/compound","home","init","--home",str(home)],capture_output=True,text=True,cwd=repo)
            self.assertEqual(init.returncode,0,init.stderr)
            result=subprocess.run(["go","run","./cmd/compound","source","observe",str(obs)],capture_output=True,text=True,env=env,cwd=repo)
            self.assertEqual(result.returncode,0,result.stderr); self.assertIn("schema: ok",result.stdout); self.assertIn("no_promote:",result.stdout)

if __name__ == "__main__": unittest.main()
