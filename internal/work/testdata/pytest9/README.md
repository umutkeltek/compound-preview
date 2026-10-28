# Native pytest fixture

Captured from genuine pytest 9.0.3 with the adjacent synthetic source:

```sh
python -m pytest -q --noconftest -p no:cacheprovider --rootdir=. \\
  --runxfail -o junit_family=xunit1 --junitxml=native.junit.xml test_native.py
```

The native report has four leaves: three passed and one explicitly skipped.
Only hostname, timestamp and the absolute location in the skip diagnostic were
normalized. File/class/name identities, outcomes, line numbers, structure and
native counts were not translated or invented. This fixture tests the parser;
it is not evidence of any product workflow or cloud/provider acceptance.
