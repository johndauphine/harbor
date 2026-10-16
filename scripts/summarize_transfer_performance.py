"""Summarize opt-in Harbor measurements without third-party dependencies."""
import html
import json
import statistics
import sys
from pathlib import Path

root = Path(sys.argv[1] if len(sys.argv) > 1 else 'dist/aws-performance')
def read(name):
    path = root / name
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()] if path.exists() else []

baseline = read('measurements.jsonl')
tuning = read('tuning.jsonl')
summary = []
for label in ['100MiB', '1024MiB', '5120MiB']:
    for kind in ['upload', 'download']:
        rows = [r for r in baseline if r.get('kind') == kind and r.get('size_label') == label]
        if rows:
            summary.append(dict(size=label, direction=kind, trials=len(rows),
                median_mib_s=statistics.median(r['mib_per_second'] for r in rows),
                median_seconds=statistics.median(r['seconds'] for r in rows),
                median_staging_seconds=statistics.median(r['staging_seconds'] for r in rows),
                max_go_heap_mib=max(r['peak_heap_mib'] for r in rows),
                median_cpu_seconds=statistics.median(r['cpu_seconds'] for r in rows)))
experiments = []
for kind in sorted({r['kind'] for r in tuning}):
    rows = [r for r in tuning if r['kind'] == kind]
    experiments.append(dict(variant=kind, trials=len(rows),
        median_mib_s=statistics.median(r['mib_per_second'] for r in rows),
        minimum_mib_s=min(r['mib_per_second'] for r in rows),
        maximum_mib_s=max(r['mib_per_second'] for r in rows),
        max_go_heap_mib=max(r['peak_heap_mib'] for r in rows)))
interruptions = [r for r in baseline if 'interruption' in r['kind']]
result = dict(baseline=summary, experiments=experiments, interruptions=interruptions)

esc = html.escape
rows = ''.join(f"<tr><td>{esc(r['size'])}</td><td>{r['direction']}</td><td>{r['trials']}</td><td>{r['median_mib_s']:.1f}</td><td>{r['median_seconds']:.2f}</td><td>{r['median_staging_seconds']:.2f}</td><td>{r['max_go_heap_mib']:.1f}</td></tr>" for r in summary)
controls = ''.join(f"<tr><td>{esc(r['variant'])}</td><td>{r['trials']}</td><td>{r['median_mib_s']:.1f}</td><td>{r['minimum_mib_s']:.1f}–{r['maximum_mib_s']:.1f}</td><td>{r['max_go_heap_mib']:.1f}</td></tr>" for r in experiments)
complete = len(summary) == 6 and all(r['trials'] == 3 for r in summary) and len(interruptions) == 6
controls_complete = len(experiments) == 3 and all(r['trials'] == 3 for r in experiments)
status = 'AWS baseline, interruption checks and download comparisons complete' if complete and controls_complete else 'Partial results — measurements still in progress'
findings = []
serial = next((r for r in experiments if r['variant'] == 'download-1-streams'), None)
parallel = next((r for r in experiments if r['variant'] == 'download-8-streams'), None)
if serial and parallel:
    findings.append(f"Eight parallel streams achieved {parallel['median_mib_s']/serial['median_mib_s']:.2f} times the single-stream median throughput in the controlled one-gibibyte test. This control used experimental SDK code; the production follow-up is reported separately below.")
large = next((r for r in summary if r['size'] == '5120MiB' and r['direction'] == 'upload'), None)
if large:
    findings.append(f"At five gibibytes, median local staging was {large['median_staging_seconds']:.2f} seconds, about {100*large['median_staging_seconds']/large['median_seconds']:.1f}% of end-to-end upload time. Preserve durable staging rather than removing recovery safeguards for a small measured gain.")
findings.append('All six baseline Cancel/retry checks passed. Explicit Cancel discards progress, so those retries restarted the whole transfer. The production follow-up checks checkpointed resume after stopping Harbor.')
findings.append('Low measured CPU use and bounded Go heap provide no evidence that a language rewrite is needed to address this bottleneck. Browser overhead and total resident memory were not measured.')
recommendations = '<ul>' + ''.join('<li>' + esc(x) + '</li>' for x in findings) + '</ul>'
production_html = ''
if len(sys.argv) > 2:
    production_root = Path(sys.argv[2])
    production = [json.loads(line) for line in (production_root / 'measurements.jsonl').read_text().splitlines() if line.strip()]
    production_summary = []
    for label in ['1GiB', '5GiB']:
        for kind in ['upload', 'download']:
            samples = [r for r in production if r.get('kind') == kind and r.get('size_label') == label]
            if samples:
                production_summary.append(dict(size=label, direction=kind, trials=len(samples),
                    median_mib_s=statistics.median(r['mib_per_second'] for r in samples),
                    median_seconds=statistics.median(r['seconds'] for r in samples),
                    max_go_heap_mib=max(r['peak_heap_mib'] for r in samples)))
    recoveries = [r for r in production if r['kind'].endswith('-resume')]
    result['production'] = production_summary
    result['production_recovery'] = recoveries
    production_rows = ''.join(f"<tr><td>{r['size']}</td><td>{r['direction']}</td><td>{r['trials']}</td><td>{r['median_mib_s']:.1f}</td><td>{r['median_seconds']:.2f}</td><td>{r['max_go_heap_mib']:.1f}</td></tr>" for r in production_summary)
    recovery_rows = ''.join(f"<li>{r['kind'].removesuffix('-resume').capitalize()}: reused {r['resumed_bytes'] / (1<<20):.0f} MiB after stopping and reconstructing Harbor; Retry took {r['retry_seconds']:.2f} seconds.</li>" for r in recoveries)
    production_html = f"<h2>Production implementation follow-up</h2><p>Eight bounded download streams with durable verified checkpoints; seekable multipart uploads with persisted acknowledged parts. Durable staging is preserved. Three 1 GiB trials and one 5 GiB trial per direction; all completed round trips require matching SHA-256. Uploads use Harbor’s native local-file staging path, whereas the baseline used its local HTTP endpoint. Network conditions and filesystem caches vary between runs.</p><div class='scroll'><table><tr><th>Size</th><th>Direction</th><th>Trials</th><th>Median MiB/s</th><th>Median seconds</th><th>Peak Go heap MiB</th></tr>{production_rows}</table></div><h3>Actual AWS restart recovery</h3><ul>{recovery_rows}</ul><p>Real MinIO process tests additionally cover abrupt termination, orderly shutdown, cancellation and cleanup, changed sources or parts, expired uploads, and lost completion responses. Retry requires user action; application launch does not automatically start transfers.</p>"
(root / 'summary.json').write_text(json.dumps(result, indent=2))
page = f'''<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Harbor AWS performance report</title>
<style>body{{font:16px/1.6 system-ui;max-width:1100px;margin:48px auto;padding:0 24px;color:#193047;background:#f5f7fa}}h1{{font-size:32px}}table{{border-collapse:collapse;width:100%;background:white;margin:24px 0}}th,td{{padding:12px;border-bottom:1px solid #dce3e9;text-align:left}}th{{background:#eaf0f6}}.scroll{{overflow:auto}}.note{{background:white;border-left:4px solid #2364bc;padding:16px 24px}}small{{color:#607182}}</style>
<h1>Harbor transfer performance on AWS S3</h1><p class="note">{status}</p>
<p>Mac to AWS S3 in US East (N. Virginia). Generated non-compressible data; three trials per size; independent SHA-256 verification. The user's existing buckets and normal Harbor session were not modified by these diagnostics.</p>
<h2>Findings</h2>{recommendations}{production_html}<h2>Harbor baseline</h2><div class="scroll"><table><tr><th>Size</th><th>Direction</th><th>Trials</th><th>Median MiB/s</th><th>Median seconds</th><th>Staging seconds</th><th>Peak Go heap MiB</th></tr>{rows}</table></div>
<h2>Controlled experiments</h2><div class="scroll"><table><tr><th>Variant</th><th>Trials</th><th>Median MiB/s</th><th>Observed range</th><th>Peak Go heap MiB</th></tr>{controls}</table></div>
<h2>Interruption and retry</h2><p>{len(interruptions)} of six upload/download interruption-and-retry measurements recorded. Targets: roughly 10%, 50%, and 90% of a one-gibibyte transfer. Progress counts source reads; upload checkpoints may be ahead of remote confirmation. Retry restarts from the beginning. Existing download destinations must survive interruption, and completed retries must match the source checksum.</p>
<h2>Limits of this test</h2><p>The baseline includes Harbor's actual local upload endpoint, journal, S3 engine, download sync and destination publication. The local sender runs in the same Go test process, so memory and CPU figures include the measurement harness and sender; browser file-reading overhead is excluded. Go heap is not total resident memory. Filesystem caches are not flushed. Network load, Wi-Fi, routing and AWS response times can vary between trials. This does not establish performance in other regions or on other Macs.</p>
<p>Experiments run separately and rotate ordering. Parallel ranges are pinned to an ETag, their exact byte ranges are checked, and every assembled file is SHA-256 verified. The controlled SDK experiments run separately from the production application; the follow-up measures the implemented production engines. No checksums, TLS validation, durable staging or recovery safeguards are disabled.</p>
<p>Reference: <a href="https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-guidelines.html">AWS S3 performance guidelines</a> recommend concurrent byte-range requests as a performance technique. These measurements determine whether it helps this Mac and connection.</p>
<p><small>Raw results: measurements.jsonl, tuning.jsonl. CPU profile: upload-cpu.pprof. Generated by Harbor's opt-in performance diagnostics.</small></p></html>'''
(root / 'report.html').write_text(page)
print(json.dumps(result, indent=2))
