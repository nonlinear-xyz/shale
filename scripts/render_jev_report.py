#!/usr/bin/env python3
"""Create an offline HTML report from a run or compare result JSON.

python3 scripts/render_jev_report.py --results /path/result.json --output /path/comparison.html
Optional --suite adds local task descriptions and source titles for cited refs.
"""
import argparse
import json
import pathlib


def render(result, suite=None):
    data = json.dumps({'result': result, 'suite': suite}).replace('<', '\\u003c').replace('>', '\\u003e').replace('&', '\\u0026')
    template = pathlib.Path(__file__).with_name('jev_report.html').read_text()
    return template.replace('__REPORT_JSON__', data)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--results', required=True, type=pathlib.Path)
    parser.add_argument('--suite', type=pathlib.Path)
    parser.add_argument('--output', required=True, type=pathlib.Path)
    args = parser.parse_args()
    result = json.loads(args.results.read_text())
    suite = json.loads(args.suite.read_text()) if args.suite else None
    with args.output.open('x') as file:
        file.write(render(result, suite))
    args.output.chmod(0o600)
