"""Linux acceptance of the shipped dispatch and filesystem transaction.

Only the external host/database observer is synthetic. The filesystem owner,
request decoder, password parser, payload construction, fsync and NOREPLACE are
exact production code. No injected final file contents or installer backend.
"""
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from urllib.parse import quote, urlunsplit

HERE = Path(__file__).resolve().parent

def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

engine = load("installer", HERE / "runtime_config_install_program.py")
verifier = load("verifier", HERE / "credential_verify_program.py")

class Observer:
    def __init__(self, crash=None, callback=None):
        self.crash, self.callback, self.boundaries = crash, callback, []
    def check(self, request):
        return dict(request["host"])
    def boundary(self, name):
        self.boundaries.append(name)
        if self.callback:
            self.callback(name)
        if name == self.crash:
            os._exit(99)

def fixture(parent):
    root = Path(parent) / "runtime"
    root.mkdir(mode=0o700)
    def synthetic_url(role,password):
        return urlunsplit(("postgresql",role+":"+quote(password,safe="")+"@origin-db:5432","/fixture","",""))
    original = ("APP_URL="+synthetic_url("fixture_app","app@synthetic")+"\nMIGRATOR_URL="+synthetic_url("fixture_migrator","migration:synthetic")+"\n").encode()
    for name, body in ((".env", original), ("inactive", b"inactive\n"), ("install.lock", b"locked-owner\n")):
        (root / name).write_bytes(body)
        (root / name).chmod(0o600)
    selected = {"DEALER_KEY": "synthetic_dealer", "PROVIDER_KEY":"synthetic_provider", "USER_AGENT":"fixture agent/1.0"}
    names = [".env", "service.env", "migration.env", "frontend.env"]
    profile = dict(ref="artifact:synthetic",sha256="a"*64,basenames=names,app_key="APP_URL",api_key="DATABASE_URL",migrator_key="MIGRATION_URL",production_key="MODE",live_fetch_key="FETCH_ENABLED",checkout_key="CHECKOUT_ENABLED",api_fields=sorted(selected),web_api_key="API_BASE",web_dealer_key="DEALER_KEY",web_cookie_key="COOKIE_NAME",web_api_base="http://fixture-api:9000",web_cookie_name="__Host-fixture",web_dealer_source="DEALER_KEY",containers=["fixture-api","fixture-web"])
    profile["api_endpoint"] = None
    image = lambda name: engine.fingerprint((root/name).stat(), (root/name).read_bytes())
    stat = root.stat()
    request = dict(schema_version=engine.SCHEMA,profile_id="synthetic",attempt_id="20261020t000000z-0123456789abcdef",program_sha256="a"*64,effect_set="inactive-aws-staging-runtime-configuration-v2",witness={},resource={},ssh={},host=dict(hostname="fixture-host",machine_id="a"*32,boot_id="abcdefab-cdef-4abc-8def-abcdefabcdef",system_identifier="1234567890123456789",version_num="170006"),files=[dict(slot=slot,basename=name,path=str(root/name),exists=i==0,stat=image(name) if i==0 else None) for i,(slot,name) in enumerate(zip(engine.SLOTS,names))],roles=[dict(kind=kind,key=key,name=name,source_host="origin-db",source_database="fixture") for kind,key,name in [("application","APP_URL","fixture_app"),("migrator","MIGRATOR_URL","fixture_migrator")]],database=dict(name="fixture",allow_connections=True,endpoint_host="target-db",endpoint_port=5432,scheme="postgresql"),posture=dict(production=True,live_fetch=False,checkout_enabled=False),forced_inactive=True,root=dict(path=str(root),device=stat.st_dev,inode=stat.st_ino,mode="0700"),marker=dict(basename="inactive",stat=image("inactive")),lock=dict(basename="install.lock",stat=image("install.lock")),profile=profile,capture=dict(projection_sha256=engine.digest(engine.encoded(selected))),qualification={})
    return request, selected, original

def invoke(request, selected, operation="install", observer=None):
    return engine.dispatch(dict(request=request,selected=selected if operation=="install" else {},operation=operation,issued_ns="1",deadline_ns="2",alignment_acceptances=[]),observer or Observer(),engine.Files(request["root"]),verifier)

def model_url(octets=(100, 64, 0, 1), port=11434):
    host = ".".join(str(n) for n in octets)
    return urlunsplit(("http", f"{host}:{port}", "", "", ""))

@unittest.skipUnless(sys.platform.startswith("linux") and os.geteuid()==0,"actual Linux root filesystem acceptance")
class TransactionAcceptance(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="rci-",dir="/opt")
        self.r,self.selected,self.original = fixture(self.temp.name)
        self.root = Path(self.r["root"]["path"])
    def tearDown(self):
        self.temp.cleanup()
    def test_private_model_endpoint_is_bound_and_only_changes_api_routing(self):
        endpoint = dict(key="FIXTURE_OLLAMA_URL", url=model_url())
        self.r["profile"]["api_endpoint"] = endpoint
        result = invoke(self.r, self.selected)
        self.assertEqual(result["state"], "installed_inactive")
        api = (self.root/"service.env").read_text()
        self.assertIn("FIXTURE_OLLAMA_URL=" + model_url() + "\n", api)
        self.assertIn("PROVIDER_KEY=synthetic_provider\n", api)
        self.assertIn("FETCH_ENABLED=false\n", api)
        for name in (".env", "migration.env", "frontend.env"):
            self.assertNotIn("FIXTURE_OLLAMA_URL", (self.root/name).read_text())
        self.assertEqual(invoke(self.r, {}, "reconcile")["state"], "installed_inactive")
        self.assertEqual(invoke(self.r, {}, "rollback")["state"], "rolled_back")
        self.assertEqual((self.root/".env").read_bytes(), self.original)
    def test_invalid_model_endpoint_refuses_before_filesystem_effects(self):
        for key, url in [("DATABASE_URL", model_url()),
                         ("FIXTURE_OLLAMA_URL", model_url((203, 0, 113, 1))),
                         ("FIXTURE_OLLAMA_URL", model_url() + "/"),
                         ("FIXTURE_OLLAMA_URL", model_url(port=5432))]:
            with self.subTest(key=key, url=url):
                self.r["profile"]["api_endpoint"] = dict(key=key, url=url)
                with self.assertRaises(engine.Refusal): invoke(self.r, self.selected)
                self.assertEqual((self.root/".env").read_bytes(), self.original)
                self.assertFalse((self.root/"service.env").exists())
    def test_actual_install_and_fresh_reconcile_and_rollback(self):
        # Unrelated operator-owned auth assets are preserved.
        (self.root/"auth.env").write_text("synthetic unrelated\n")
        result=invoke(self.r,self.selected)
        self.assertEqual(result["state"],"installed_inactive")
        self.assertTrue(result["configuration_accepted"])
        expected=urlunsplit(("postgresql","fixture_app:app%40synthetic@target-db:5432","/fixture","",""))
        self.assertEqual((self.root/".env").read_text(),"APP_URL="+expected+"\n")
        api=(self.root/"service.env").read_text()
        self.assertIn("FETCH_ENABLED=false\n",api)
        self.assertIn("CHECKOUT_ENABLED=false\n",api)
        self.assertIn("USER_AGENT='fixture agent/1.0'\n",api)
        self.assertNotIn("migration%3Asynthetic",api)
        self.assertEqual(invoke(self.r,{},"reconcile")["state"],"installed_inactive")
        self.assertEqual(invoke(self.r,{},"rollback")["state"],"rolled_back")
        self.assertEqual((self.root/".env").read_bytes(),self.original)
        for name in self.r["profile"]["basenames"][1:]: self.assertFalse((self.root/name).exists())
        self.assertEqual((self.root/"auth.env").read_text(),"synthetic unrelated\n")
        self.assertEqual(invoke(self.r,{},"rollback")["state"],"rolled_back")
    def test_recovery_before_first_remote_effect_proves_unchanged(self):
        result=invoke(self.r,{},"rollback")
        self.assertEqual(result["state"],"rolled_back")
        self.assertFalse(result["changed"])
        self.assertEqual((self.root/".env").read_bytes(),self.original)
        self.assertFalse((self.root/(".rci-"+self.r["attempt_id"])).exists())
    def test_staged_drift_after_publish_intent_refuses_target_write(self):
        def corrupt(name):
            if name=="publish-intent-app":
                (self.root/(".rci-"+self.r["attempt_id"])/"stage-app").write_bytes(b"DRIFT=untrusted\n")
        with self.assertRaises(engine.Refusal):invoke(self.r,self.selected,observer=Observer(callback=corrupt))
        self.assertEqual((self.root/".env").read_bytes(),self.original)
        self.assertFalse((self.root/"service.env").exists())
    def test_every_real_boundary_survives_process_death(self):
        observer=Observer()
        invoke(self.r,self.selected,observer=observer)
        boundaries=observer.boundaries
        self.assertIn("manifest",boundaries)
        self.assertIn("rename-api",boundaries)
        for boundary in boundaries:
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory(prefix="rci-crash-",dir="/opt") as temp:
                request,selected,original=fixture(temp)
                fixture_path=Path(temp)/"fixture.json"
                fixture_path.write_text(json.dumps([request,selected]))
                child=subprocess.run([sys.executable,__file__,"--crash",str(fixture_path),boundary],capture_output=True)
                self.assertEqual(child.returncode,99,child.stderr.decode())
                self.assertEqual(invoke(request,{},"rollback")["state"],"rolled_back")
                root=Path(request["root"]["path"])
                self.assertEqual((root/".env").read_bytes(),original)
                for name in request["profile"]["basenames"][1:]: self.assertFalse((root/name).exists())
    def test_every_rollback_boundary_survives_process_death(self):
        invoke(self.r,self.selected)
        observer=Observer()
        invoke(self.r,{},"rollback",observer)
        self.assertIn("restore-stage",observer.boundaries)
        for boundary in observer.boundaries:
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory(prefix="rci-undo-",dir="/opt") as temp:
                request,selected,original=fixture(temp)
                invoke(request,selected)
                fixture_path=Path(temp)/"fixture.json"
                fixture_path.write_text(json.dumps([request,selected]))
                child=subprocess.run([sys.executable,__file__,"--crash",str(fixture_path),boundary,"rollback"],capture_output=True)
                self.assertEqual(child.returncode,99,child.stderr.decode())
                self.assertEqual(invoke(request,{},"rollback")["state"],"rolled_back")
                self.assertEqual((Path(request["root"]["path"])/".env").read_bytes(),original)
    def test_every_fsync_failure_retains_recoverable_transaction(self):
        real=engine.os.fsync
        calls=[0]
        def count(fd):
            calls[0]+=1
            real(fd)
        engine.os.fsync=count
        try: invoke(self.r,self.selected)
        finally: engine.os.fsync=real
        total=calls[0]
        self.assertGreater(total,30)
        for failure in range(1,total+1):
            with self.subTest(fsync=failure), tempfile.TemporaryDirectory(prefix="rci-sync-",dir="/opt") as temp:
                request,selected,original=fixture(temp)
                calls[0]=0
                def fail(fd):
                    calls[0]+=1
                    if calls[0]==failure: raise OSError("synthetic fsync failure")
                    real(fd)
                engine.os.fsync=fail
                try:
                    with self.assertRaises(OSError): invoke(request,selected)
                finally: engine.os.fsync=real
                root=Path(request["root"]["path"])
                # Failure before recovery directory creation proves zero effects.
                if (root/(".rci-"+request["attempt_id"])).exists():
                    self.assertEqual(invoke(request,{},"rollback")["state"],"rolled_back")
                self.assertEqual((root/".env").read_bytes(),original)
                for name in request["profile"]["basenames"][1:]: self.assertFalse((root/name).exists())
    def test_anchored_root_replacement_between_manifest_and_publish(self):
        retired=self.root.with_name("retired")
        def swap(name):
            if name=="manifest":
                self.root.rename(retired)
                self.root.mkdir(mode=0o700)
        with self.assertRaises(engine.Refusal): invoke(self.r,self.selected,observer=Observer(callback=swap))
        self.assertEqual((retired/".env").read_bytes(),self.original)
        self.assertFalse((retired/"service.env").exists())
        self.assertEqual(list(self.root.iterdir()),[])
    def test_drift_in_any_slot_prevents_all_recovery_writes(self):
        invoke(self.r,self.selected)
        (self.root/"frontend.env").write_text("operator drift\n")
        before={p.name:p.read_bytes() for p in self.root.iterdir() if p.is_file()}
        with self.assertRaises(engine.Refusal): invoke(self.r,{},"rollback")
        after={p.name:p.read_bytes() for p in self.root.iterdir() if p.is_file()}
        self.assertEqual(before,after)
    def test_mode_hardlink_symlink_and_parent_replacement_refuse(self):
        for mutation in ("mode","hardlink","symlink","parent"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory(prefix="rci-drift-",dir="/opt") as temp:
                request,selected,_=fixture(temp)
                root=Path(request["root"]["path"])
                if mutation=="mode": (root/".env").chmod(0o644)
                elif mutation=="hardlink": os.link(root/".env",root/"alias")
                elif mutation=="symlink":
                    (root/".env").rename(root/"real")
                    (root/".env").symlink_to("real")
                else:
                    root.rename(root.with_name("retired"))
                    root.mkdir(mode=0o700)
                with self.assertRaises((engine.Refusal,OSError)): invoke(request,selected)
                self.assertFalse((root/"service.env").exists())
    def test_short_writes_are_completed_without_truncation(self):
        actual=engine.os.write
        engine.os.write=lambda fd,data:actual(fd,data[:3])
        try: result=invoke(self.r,self.selected)
        finally: engine.os.write=actual
        self.assertEqual(result["state"],"installed_inactive")
        self.assertIn("synthetic_provider",(self.root/"service.env").read_text())

    def test_partial_journal_write_never_publishes_invalid_record(self):
        actual=engine.os.write
        partial=[False]
        def fail(fd,data):
            path=os.readlink("/proc/self/fd/"+str(fd))
            if "/.pending-" in path:
                if partial[0]: raise OSError("synthetic partial journal write")
                partial[0]=True
                return actual(fd,data[:3])
            return actual(fd,data)
        engine.os.write=fail
        try:
            with self.assertRaises(OSError): invoke(self.r,self.selected)
        finally: engine.os.write=actual
        self.assertTrue(partial[0])
        recovery=self.root/(".rci-"+self.r["attempt_id"])
        self.assertFalse((recovery/"manifest.json").exists())
        result=invoke(self.r,self.selected,"rollback")
        self.assertEqual(result["state"],"rolled_back")
        self.assertFalse(result["changed"])
    def test_noreplace_never_overwrites_companion(self):
        a=self.root/"a";b=self.root/"b"
        a.write_bytes(b"source");b.write_bytes(b"operator")
        fd=os.open(self.root,os.O_RDONLY|os.O_DIRECTORY)
        try:
            with self.assertRaises(OSError): engine.rename_noreplace("a",fd,"b",fd)
        finally: os.close(fd)
        self.assertEqual(a.read_bytes(),b"source")
        self.assertEqual(b.read_bytes(),b"operator")

if __name__=="__main__":
    if len(sys.argv)>1 and sys.argv[1]=="--crash":
        request,selected=json.loads(Path(sys.argv[2]).read_text())
        invoke(request,selected,sys.argv[4] if len(sys.argv)>4 else "install",observer=Observer(crash=sys.argv[3]))
        raise SystemExit(3)
    unittest.main()
