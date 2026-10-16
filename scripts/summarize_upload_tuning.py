"""Summarize the opt-in SDK and journaled-engine upload experiments."""
import html
import json
import statistics
import sys
from pathlib import Path

sdk_root = Path(sys.argv[1] if len(sys.argv) > 1 else 'dist/aws-upload-tuning')
engine_root = Path(sys.argv[2] if len(sys.argv) > 2 else 'dist/aws-upload-engine')

def records(root, name):
    path = root / name
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()] if path.exists() else []

def summarize(samples):
    rows = []
    groups = sorted({(r['size_label'], r['kind']) for r in samples})
    for size, variant in groups:
        group = [r for r in samples if r['size_label'] == size and r['kind'] == variant]
        rows.append(dict(size=size, variant=variant, trials=len(group),
            median_mib_s=statistics.median(r['mib_per_second'] for r in group),
            minimum_mib_s=min(r['mib_per_second'] for r in group),
            maximum_mib_s=max(r['mib_per_second'] for r in group),
            median_seconds=statistics.median(r['seconds'] for r in group),
            peak_heap_mib=max(r['peak_heap_mib'] for r in group)))
    return rows

def table(rows):
    body = ''.join(f"<tr><td>{html.escape(r['size'])}</td><td>{html.escape(r['variant'])}</td><td>{r['trials']}</td><td>{r['median_mib_s']:.1f}</td><td>{r['minimum_mib_s']:.1f}–{r['maximum_mib_s']:.1f}</td><td>{r['median_seconds']:.2f}</td><td>{r['peak_heap_mib']:.1f}</td></tr>" for r in rows)
    return f'<div class="scroll"><table><tr><th>Size</th><th>Settings</th><th>Trials</th><th>Median MiB/s</th><th>Range MiB/s</th><th>Median seconds</th><th>Peak Go heap MiB</th></tr>{body}</table></div>'

sdk = summarize(records(sdk_root, 'tuning.jsonl'))
engine = summarize(records(engine_root, 'measurements.jsonl'))
recovery_path = engine_root / 'recovery.json'
recovery = json.loads(recovery_path.read_text()) if recovery_path.exists() else {}
result = dict(sdk=sdk, engine=engine, recovery=recovery)
(sdk_root / 'summary.json').write_text(json.dumps(result, indent=2))
complete = all((root / 'run.log').exists() and '\nPASS\n' in (root / 'run.log').read_text() for root in [sdk_root, engine_root])
complete = complete and recovery.get('sha256_verified', False)
status = 'AWS comparison and restart recovery complete; all completed objects verified by SHA-256' if complete else 'Partial measurements; validation still in progress'
baseline = next((r for r in engine if r['size'] == '1024MiB' and r['variant'].startswith('baseline')), None)
candidate = next((r for r in engine if r['size'] == '1024MiB' and r['variant'].startswith('candidate')), None)
finding = ''
if baseline and candidate:
    finding = f"<p>The journaled-engine candidate reached {candidate['median_mib_s']/baseline['median_mib_s']:.2f} times the baseline median throughput across three 1 GiB trials per setting. The separate 5 GiB candidate run checks scale; it is a single trial.</p>"
recovery_html = f"<p>The tuned upload reused {recovery['resumed_bytes']/(1<<20):.0f} MiB after app shutdown/reconstruction; Retry took {recovery['retry_seconds']:.2f} seconds and the complete recovered object matched SHA-256.</p>" if recovery.get('sha256_verified') else ''
page = f'''<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Harbor upload tuning</title>
<style>body{{font:16px/1.6 system-ui;max-width:1200px;margin:40px auto;padding:0 24px;color:#193047;background:#f5f7fa}}h1{{font-size:32px}}table{{border-collapse:collapse;width:100%;background:white;margin:24px 0}}th,td{{padding:10px;border-bottom:1px solid #dce3e9;text-align:left}}th{{background:#eaf0f6}}.scroll{{overflow:auto}}.note{{background:white;border-left:4px solid #2364bc;padding:16px 24px}}</style>
<h1>Harbor upload speed tuning</h1><p class="note">{status}</p>{finding}
<h2>SDK screening: concurrency and part size</h2><p>512 MiB of non-compressible data; three rotated trials per setting. Five or eight workers, with 5, 16 or 32 MiB multipart parts. Each remote object is independently downloaded and SHA-256 checked outside the upload timing.</p>{table(sdk)}
<h2>Production upload engine</h2><p>Rotated comparisons of the journaled engine include local staging, staged-source hashing, acknowledged-part checkpoints, and S3 completion. Three 1 GiB trials per setting, plus one 5 GiB candidate check. Uploads retain seekable source reads and the shared eight-request limit.</p>{table(engine)}
<h2>Recovery and limits</h2>{recovery_html}<p>Durable staging remains enabled. Recovered uploads use the part size saved in their existing checkpoint, including earlier 5 MiB uploads. Explicit Cancel discards progress; network failure or application shutdown retains verified parts for Retry. Larger parts trade fewer requests for more unconfirmed work during interruptions; the chosen 16 MiB setting avoids doubling that recovery unit when 32 MiB offers no clear speed advantage.</p>
<p>Results apply to this Mac and AWS S3 connection in US East (N. Virginia). Trial order is rotated; filesystem caches are not flushed, and network conditions can vary. Go heap is not total resident memory. Browser file-reading overhead is excluded. Tests use isolated temporary buckets and journals and do not modify user connections or existing storage.</p></html>'''
(sdk_root / 'report.html').write_text(page)
print(json.dumps(result, indent=2))
