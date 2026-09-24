import json
import unittest
from render_jev_review import render


class ReviewTest(unittest.TestCase):
    def test_embedded_source_cannot_escape_json(self):
        suite = {'version': 1, 'cases': [], 'note': '</script><script>alert(1)</script>'}
        page = render(suite)
        data = page.split('<script id="suite" type="application/json">', 1)[1].split('</script>', 1)[0]
        self.assertNotIn('<script>', data)
        self.assertEqual(json.loads(data), suite)
        self.assertIn('textContent', page)


if __name__ == '__main__':
    unittest.main()
