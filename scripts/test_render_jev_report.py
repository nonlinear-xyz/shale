import json
import unittest
from render_jev_report import render


class ReportTest(unittest.TestCase):
    def test_source_json_is_inert(self):
        result = {'attempts': [], 'note': '</script><img src=x onerror=alert(1)>'}
        page = render(result)
        data = page.split('<script id="data" type="application/json">', 1)[1].split('</script>', 1)[0]
        self.assertEqual(json.loads(data)['result'], result)
        self.assertNotIn('<img', data)
        self.assertNotIn('__REPORT_JSON__', page)


if __name__ == '__main__':
    unittest.main()
