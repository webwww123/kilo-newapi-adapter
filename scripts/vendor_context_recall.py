#!/usr/bin/env python3
"""Vendor the verified selector into another Go middleware without a runtime dependency."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess

REPO = Path(__file__).resolve().parents[1]
REVISION = 'eb0755e88e653867cc9f48952b5db6c30f1f8fb7'


def source(name):
    return subprocess.check_output(['git', 'show', REVISION + ':internal/adapter/' + name], cwd=REPO, text=True)


def build(destination, local_estimator=False):
    original = {name: source(name) for name in ('context_recall.go', 'context_recall_control.go', 'context_trim.go', 'context_recall_fragments.go', 'context_recall_repeated.go', 'context_recall_repeated_test.go', 'context_recall_repeated_control_test.go')}
    core = original['context_recall.go'].replace('package adapter', 'package contextrecall', 1)
    fragments = original['context_recall_fragments.go'].replace('package adapter', 'package contextrecall', 1)
    fragments = fragments.replace('contextRecallRequest', 'Request').replace('selectTextItems(', 'SelectTextItems(').replace('selectText(', 'SelectText(')
    repeated = original['context_recall_repeated.go'].replace('package adapter', 'package contextrecall', 1).replace('contextRecallRequest', 'Request')
    helper = original['context_trim.go']
    functions = {}
    for match in re.finditer(r'(?m)^func (\w+)\(', helper):
        end = helper.index('\n}', match.start()) + 2
        functions[match.group(1)] = helper[match.start():end]
    wanted = set(re.findall(r'\b\w+\b', core + fragments + repeated)) & functions.keys()
    while True:
        expanded = wanted | (set(re.findall(r'\b\w+\b', '\n'.join(functions[n] for n in wanted))) & functions.keys())
        if expanded == wanted:
            break
        wanted = expanded
    safety = ('package contextrecall\n\nimport ("encoding/json"; "fmt"; "math"; "sort"; "strings")\n\n'
              'type contextItemGroup struct { indexes []int }\n\n' +
              '\n\n'.join(functions[n] for n in functions if n in wanted) + '\n')
    control = original['context_recall_control.go'].replace('package adapter', 'package contextrecall', 1)
    names = {'contextRecallControl':'Control', 'contextRecallRequest':'Request',
             'newContextRecallControl':'NewControl'}
    for old, new in names.items():
        control = re.sub(r'\b' + old + r'\b', new, control)
    control = control.replace('(c *Control) begin(', '(c *Control) Begin(')
    control = control.replace('(q *Request) selectItems(', '(q *Request) SelectItems(')
    control = control.replace('(q *Request) finish(', '(q *Request) Finish(')
    control = control.replace('engine: newContextRecallEngine()', 'engine: sharedRecallEngine')
    control = control.replace('"sync"', '"sync"\n"strings"')
    control = control.replace('normalizePath(r.URL.Path)', 'strings.TrimPrefix(strings.TrimRight(r.URL.Path, "/"), "/v1")')
    control += '''
// Multiple branch handlers share the same two non-queueing process slots.
var sharedRecallEngine = newContextRecallEngine()

func (q *Request) SetInputBytes(n int) {
    if q != nil { q.event.InputBytes = n }
}

func WithRequest(ctx context.Context, q *Request) context.Context {
    if q == nil { return ctx }
    return context.WithValue(ctx, contextRecallRequestKey{}, q)
}

func FromContext(ctx context.Context) *Request {
    q, _ := ctx.Value(contextRecallRequestKey{}).(*Request)
    return q
}
'''
    if local_estimator:
        fragments = fragments.replace('return contextEstimateAnyTokens(value)', 'return r.estimateItem(value)')
        core = core.replace('type contextRecallEngine struct {', 'type contextRecallEngine struct {\n estimate func(any) int')
        core = core.replace('costs[i] = contextEstimateAnyTokens(item) + 4', 'costs[i] = r.estimateItem(item) + 4')
        core = core.replace('if contextEstimateItemsTokens(kept) > budget', 'if r.estimateItems(kept) > budget')
        core += '''
// Preserve the host middleware's existing token estimator and input budget.
func (r *contextRecallEngine) estimateItem(item any) int {
    if r.estimate != nil { return r.estimate(item) }
    return contextEstimateAnyTokens(item)
}
func (r *contextRecallEngine) estimateItems(items []any) int {
    total := 0
    for _, item := range items { total += r.estimateItem(item) + 4 }
    return total
}
'''
        control += '''
func NewControlWithEstimator(path string, estimate func(any) int) *Control {
    c := NewControl(path)
    if c == nil { return nil }
    engine := *c.engine
    engine.estimate = estimate
    c.engine = &engine // The process-wide non-queueing slot channel stays shared.
    return c
}
'''
    destination = Path(destination)
    destination.mkdir(parents=True, exist_ok=True)
    manifest_path = destination / 'provenance.json'
    previous = json.loads(manifest_path.read_text()) if manifest_path.exists() else None
    if previous:
        for name, digest in previous['output_sha256'].items():
            if hashlib.sha256((destination/name).read_bytes()).hexdigest() != digest:
                raise ValueError('existing generated source was edited')
    header = '// Code generated by the verified context-recall vendor workflow; DO NOT EDIT.\n'
    header += '// Selector source: ' + REVISION + '. No model calls or network dependency.\n'
    tests = original['context_recall_repeated_test.go'].split('func TestRepeatedLineFullPathPreservesExistingValue', 1)[0]
    policy_test = original['context_recall_repeated_control_test.go'].split('func TestContextRecallRepeatedHotDisablePreservesExistingPolicy', 1)[1]
    tests = tests.replace('package adapter', 'package contextrecall', 1).replace('"context"', '"context"\n"encoding/json"\n"os"\n"path/filepath"')
    tests += '\nfunc TestContextRecallRepeatedHotDisablePreservesExistingPolicy' + policy_test.replace('newContextRecallControl', 'NewControl')
    outputs = {'selector.go': core, 'control.go': control, 'safety.go': safety, 'fragments.go': fragments, 'repeated.go': repeated, 'repeated_test.go': tests}
    for name, value in outputs.items():
        target = destination / name
        if target.exists() and (previous is None or name not in previous['output_sha256']):
            raise ValueError('refusing to overwrite existing vendored source')
        target.write_text(header + value)
        subprocess.run(['gofmt', '-w', str(target)], check=True)
    manifest = {'revision': REVISION, 'local_estimator':local_estimator, 'source_sha256': {k: hashlib.sha256(v.encode()).hexdigest() for k,v in original.items()},
                'output_sha256': {name: hashlib.sha256((destination/name).read_bytes()).hexdigest()
                                  for name in outputs}}
    manifest_path.write_text(json.dumps(manifest, indent=2)+'\n')
    print(json.dumps({'files':len(manifest['output_sha256']),'helper_functions':len(wanted),'source':REVISION}))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('destination')
    parser.add_argument('--local-estimator', action='store_true')
    args = parser.parse_args()
    build(args.destination, args.local_estimator)
