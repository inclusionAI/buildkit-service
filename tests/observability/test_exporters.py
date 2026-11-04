import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[2]


def script(name, template):
    text=(ROOT/'chart/templates'/template).read_text()
    source=text.split('{{- define "'+name+'" -}}',1)[1].split('{{- end -}}',1)[0]
    namespace={'__name__':'observability_test'}
    exec(compile(source,template,'exec'),namespace)
    return namespace


class ExporterTests(unittest.TestCase):
    def test_hardlinks_sparse_files_and_budget(self):
        exporter=script('buildkit-service.dirSizeExporterScript','_dir-size-exporter.tpl')
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'sparse'
            with path.open('wb') as f:f.truncate(1<<20)
            os.link(path,Path(directory)/'hardlink')
            exporter['TARGETS']=[('buildkit',directory)]
            with patch.dict(os.environ,{'CACHE_LIMIT_BYTES':'33554432'}):text=exporter['render']()
            self.assertIn('cache_used_bytes{target="buildkit"} 1048576',text)
            self.assertIn('cache_limit_bytes{target="buildkit"} 33554432',text)
            self.assertIn('cache_allocated_bytes{target="buildkit"} '+str(path.stat().st_blocks*512),text)
            self.assertIn('cache_scan_success{target="buildkit"} 1',text)
            self.assertIn('filesystem_available_bytes',text)

    def test_missing_directory_is_unknown_not_empty(self):
        exporter=script('buildkit-service.dirSizeExporterScript','_dir-size-exporter.tpl')
        exporter['TARGETS']=[('missing','/path/that/does/not/exist')]
        text=exporter['render']()
        self.assertIn('cache_scan_success{target="missing"} 0',text)
        self.assertNotIn('cache_used_bytes{target=',text)

    def test_incomplete_scan_is_not_an_empty_cache(self):
        exporter=script('buildkit-service.dirSizeExporterScript','_dir-size-exporter.tpl')
        with tempfile.TemporaryDirectory() as directory:
            exporter['TARGETS']=[('cache',directory)]
            def denied(path,onerror=None):
                onerror(PermissionError('denied'))
                return iter([])
            with patch.object(exporter['os'],'walk',side_effect=denied):text=exporter['render']()
            self.assertIn('cache_scan_success{target="cache"} 0',text)
            self.assertNotIn('cache_used_bytes{target=',text)

    def test_partial_scrape_is_not_healthy(self):
        aggregator=script('buildkit-service.metricsAggregatorScript','_metrics-aggregator.tpl')
        aggregator['resolve']=lambda _:['10.0.0.1','10.0.0.2']
        def fetch(url):
            if '10.0.0.2' in url:raise TimeoutError()
            return 'buildkit_service_cache_used_bytes 5\n'
        aggregator['http_get']=fetch
        out=[];up={}
        aggregator['job_dirsize_sum']({'dns':'worker','prefix':'buildkit_service','port':9300},out,up)
        self.assertEqual(up['buildkit_service'],0)
        self.assertIn('metrics_aggregator_targets_scraped{target="buildkit_service"} 1',out)
        self.assertIn('metrics_aggregator_targets_expected{target="buildkit_service"} 2',out)


if __name__=='__main__':unittest.main()
