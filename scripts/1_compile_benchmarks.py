#!/home/sbin/tools/Python3.10.14/bin/python3
import multiprocessing
import os
import subprocess
import sys

path = "../mgpusim/amd/samples/"

# TESTS = [
#     'altis_cfd',
#     'atax',
#     'babelstream',
#     'bfs',
#     'bicg',
#     'cache_latency',
#     'fastwalshtransform',
#     'fft',
#     'fir',
#     'floydwarshall',
#     'kmeans',
#     'matrixtranspose',
#     'nbody',
#     'npb_ep',
#     'parboil_cutcp',
#     'parboil_sgemm',
#     'polybench_2dconv',
#     'polybench_3dconv',
#     'polybench_3mm',
#     'polybench_correlation',
#     'polybench_fdtd2d',
#     'polybench_gemm',
#     'polybench_jacobi2d',
#     'polybench_mvt',
#     'polybench_syr2k',
#     'relu',
#     'rodinia_backprop',
#     'rodinia_gaussian',
#     'rodinia_hotspot',
#     'rodinia_hotspot3d',
#     'rodinia_lavamd',
#     'rodinia_lud',
#     'rodinia_pathfinder',
#     'rodinia_srad',
#     'spmv',
#     'stencil2d',
#     'tango_blackscholes',
#     'vectoradd',
#     'polybench_doitgen',
#     'polybench_gemver',
#     'polybench_gesummv',
#     'polybench_jacobi1d',
#     'polybench_lu',
#     'pannotia_color',
#     'pannotia_mis',
#     'pannotia_sssp',
#     'gups',
#     'reduction',
#     'graphbig_betweennesscentr',
#     'graphbig_bfs',
#     'graphbig_connectedcomp',
#     'graphbig_degreecentr',
#     'graphbig_gc',
#     'graphbig_kcore',
#     'graphbig_sssp',
#     'graphbig_trianglecount',
# ]

# sbin_claude: integrated benchmark set (v4 originals + the suites
# ported from ~/vdram_v2: altis, babelstream, graphbig, mafiaports,
# microbench, npb, pannotia, parboil, polybench, rodinia, tango).
TESTS = [
    'altis_cfd',
    'atax',
    'babelstream',
    'bfs',
    'bicg',
    'cache_latency',
    'fastwalshtransform',
    'fft',
    'fir',
    'floydwarshall',
    'graphbig_betweennesscentr',
    'graphbig_bfs',
    'graphbig_connectedcomp',
    'graphbig_degreecentr',
    'graphbig_gc',
    'graphbig_kcore',
    'graphbig_sssp',
    'graphbig_trianglecount',
    'gups',
    'kmeans',
    'matrixmultiplication',
    'matrixtranspose',
    'nbody',
    'npb_ep',
    'nw',
    'pagerank',
    'pannotia_color',
    'pannotia_mis',
    'pannotia_sssp',
    'parboil_cutcp',
    'parboil_sgemm',
    'polybench_2dconv',
    'polybench_3dconv',
    'polybench_3mm',
    'polybench_correlation',
    'polybench_doitgen',
    'polybench_fdtd2d',
    'polybench_gemm',
    'polybench_gemver',
    'polybench_gesummv',
    'polybench_jacobi1d',
    'polybench_jacobi2d',
    'polybench_lu',
    'polybench_mvt',
    'polybench_syr2k',
    'reduction',
    'relu',
    'rodinia_backprop',
    'rodinia_gaussian',
    'rodinia_hotspot',
    'rodinia_hotspot3d',
    'rodinia_lavamd',
    'rodinia_lud',
    'rodinia_pathfinder',
    'rodinia_srad',
    'simpleconvolution',
    'spmv',
    'stencil2d',
    'tango_blackscholes',
    'vectoradd',
]


class Test(object):
    """define a benchmark to test"""

    def __init__(self, path):
        self.path = path

    def compile(self):
        fp = open(os.devnull, 'w')
        p = subprocess.Popen('go build', shell=True,
                             cwd=self.path, stdout=fp, stderr=fp)
        p.wait()
        if p.returncode == 0:
            print("Compiled " + self.path, 'green')
            return False
        else:
            print("Compile failed " + self.path, 'red')
            return True


def compile_test(test):
    return test.compile()


def main():
    tests = [Test(path + t) for t in TESTS]

    worker_count = min(len(tests), multiprocessing.cpu_count())
    with multiprocessing.Pool(processes=worker_count) as pool:
        results = pool.map(compile_test, tests)

    err = any(results)
    print("Error count:", sum(results))
    if err:
        sys.exit(1)


if __name__ == '__main__':
    main()
