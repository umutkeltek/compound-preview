"""Safe-result installer fence. Every filesystem body here is synthetic."""
import importlib.util
import os
from pathlib import Path
import tempfile
import sys
import fcntl
import unittest

HERE=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location("alignment_installer_fixture",HERE/"runtime_config_install_program.py")
engine=importlib.util.module_from_spec(spec)
spec.loader.exec_module(engine)


@unittest.skipUnless(sys.platform.startswith("linux") and os.geteuid()==0,"actual Linux/root nofollow filesystem acceptance")
class AlignmentInstallerFence(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory(prefix="closed-alignment-",dir=Path.home())
        self.root=Path(self.temp.name)
        self.root.chmod(0o700)
        s=self.root.stat()
        self.files=engine.Files(dict(path=str(self.root),device=s.st_dev,inode=s.st_ino,mode="0700"),os.getuid(),os.getgid())
        self.original="act_0123456789ab"
        self.action="act_1123456789ab"
    def tearDown(self):
        self.files.close()
        self.temp.cleanup()
    def put(self,parent,name,value):
        path=parent/name
        path.write_bytes(engine.encoded(value)+b"\n")
        path.chmod(0o600)
        return path
    def observation(self,operation="align",resolved=()):
        action=self.original if operation=="align" else self.action
        return dict(schema_version="compound.credential_password_alignment_observation.v0.1",operation=operation,action_intent_id=action,original_action_intent_id=self.original,request_sha256="a"*64,classification="new_new",effect="committed" if operation=="align" else "reconciled_new_new",closed=True,protected_file_unchanged=True,attributes_unchanged=True,configuration_accepted=False,credential_qualification_accepted=False,catalog=None,resolved_recovery_actions=list(resolved))
    def acceptance(self,result):
        value={key:result[key] for key in ("action_intent_id","original_action_intent_id","request_sha256","operation","effect")}
        value["observation_sha256"]=engine.digest(engine.encoded(result))
        return value
    def custody(self):
        namespace=self.root/".compound-credential-password-alignment"
        namespace.mkdir(mode=0o700)
        original=namespace/self.original
        original.mkdir(mode=0o700)
        for name in ("PREIMAGE.json","POSTIMAGE.json","COMMIT_INTENT.json"):
            self.put(original,name,{"private_synthetic_record":True})
        return original
    def test_no_history_and_exact_completed_history(self):
        engine.alignment_fence(self.files,[])
        original=self.custody()
        result=self.observation()
        self.put(original,"RESULT.json",result)
        read=self.files.read
        observed=[]
        def safe_read(parent,name,*args,**kwargs):
            observed.append(name)
            self.assertEqual(name,"RESULT.json","private verifier body was opened")
            return read(parent,name,*args,**kwargs)
        self.files.read=safe_read
        engine.alignment_fence(self.files,[self.acceptance(result)])
        self.assertEqual(observed,["RESULT.json"])
    def test_remote_unknown_refuses_even_without_local_records_or_with_later_auth(self):
        original=self.custody()
        for accepted in ([],[self.acceptance(self.observation())]):
            with self.assertRaises((engine.Refusal,FileNotFoundError)):
                engine.alignment_fence(self.files,accepted)
        result=self.observation()
        self.put(original,"RESULT.json",result)
        recovery=original/("recovery_"+self.action)
        recovery.mkdir(mode=0o700)
        self.put(recovery,"PREIMAGE.json",{"private_synthetic_record":True})
        with self.assertRaises(engine.Refusal):
            engine.alignment_fence(self.files,[self.acceptance(result)])
    def test_result_substitution_missing_preimage_and_link_tampering_refuse(self):
        original=self.custody()
        result=self.observation()
        safe=self.put(original,"RESULT.json",result)
        accepted=[self.acceptance(result)]
        result["request_sha256"]="b"*64
        self.put(original,"RESULT.json",result)
        with self.assertRaises(engine.Refusal): engine.alignment_fence(self.files,accepted)
        result["request_sha256"]="a"*64
        self.put(original,"RESULT.json",result)
        os.link(safe,original/"alias")
        with self.assertRaises(engine.Refusal): engine.alignment_fence(self.files,accepted)
        os.unlink(original/"alias")
        os.unlink(original/"PREIMAGE.json")
        with self.assertRaises(engine.Refusal): engine.alignment_fence(self.files,accepted)
        self.put(original,"PREIMAGE.json",{})
        os.unlink(safe)
        os.symlink(original/"PREIMAGE.json",safe)
        with self.assertRaises((engine.Refusal,OSError)): engine.alignment_fence(self.files,accepted)
    def test_reconciliation_must_cover_every_prior_recovery_and_no_later_one(self):
        original=self.custody()
        previous="act_2123456789ab"
        for action in (previous,self.action):
            directory=original/("recovery_"+action)
            directory.mkdir(mode=0o700)
            self.put(directory,"PREIMAGE.json",{"private_synthetic_record":True})
        result=self.observation("reconcile",[previous])
        self.put(original/("recovery_"+self.action),"RESULT.json",result)
        accepted=[self.acceptance(result)]
        engine.alignment_fence(self.files,accepted)
        later=original/"recovery_act_3123456789ab"
        later.mkdir(mode=0o700)
        self.put(later,"PREIMAGE.json",{"private_synthetic_record":True})
        with self.assertRaises(engine.Refusal): engine.alignment_fence(self.files,accepted)

    def test_installer_holds_same_namespace_lock_for_its_lifetime(self):
        original=self.custody()
        namespace=original.parent
        transaction=engine.Transaction(dict(attempt_id="synthetic"),None,engine.Files(self.files.root,os.getuid(),os.getgid()))
        closed=False
        held=os.open(namespace,os.O_RDONLY|os.O_DIRECTORY)
        try:
            fcntl.flock(held,fcntl.LOCK_EX|fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):transaction.lock_alignment()
            fcntl.flock(held,fcntl.LOCK_UN)
            transaction.lock_alignment()
            with self.assertRaises(BlockingIOError):fcntl.flock(held,fcntl.LOCK_EX|fcntl.LOCK_NB)
            transaction.close()
            closed=True
            fcntl.flock(held,fcntl.LOCK_EX|fcntl.LOCK_NB)
        finally:
            if not closed:transaction.close()
            os.close(held)


if __name__=="__main__":unittest.main()
