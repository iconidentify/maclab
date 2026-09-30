"""Rebuild the UI's web fonts: JetBrains Mono Nerd Font (Omarchy's default)
subset to text plus the icons app.js uses, and IBM Plex Sans subset to Latin.
Run: uv run --with fonttools --with brotli python3 scripts/fonts.py <path to IBMPlexSans[wdth,wght].ttf>
The Plex subset is renamed "Lab Sans": "Plex" is a Reserved Font Name under its
OFL, and a subset is a modified version."""
import re, sys
from fontTools import subset
from fontTools.ttLib import TTFont

js = open('internal/web/static/app.js').read()
icons = {int(h, 16) for h in re.findall(r"\\u\{([0-9A-Fa-f]+)\}", js)}
mono = '/usr/share/fonts/TTF/JetBrainsMonoNerdFont-Regular.ttf'
names = {cp: n for cp, n in TTFont(mono).getBestCmap().items()}
for name, cp in sorted(re.findall(r"(\w+): '\\u\{([0-9A-Fa-f]+)\}'", js)):
    print(f"  {name:10} U+{cp}  {names.get(int(cp, 16), 'MISSING')}")
text = set(range(0x20, 0x7f)) | set(range(0xa0, 0x180)) | set(range(0x2000, 0x2070)) | set(range(0x2190, 0x21ff)) | set(range(0x2500, 0x25ff))
def rename(f, family):
    for n in f['name'].names:
        if n.nameID in (1, 4, 16, 21):
            n.string = family + (' Regular' if n.nameID == 4 else '')
        elif n.nameID in (3, 6):
            n.string = family.replace(' ', '') + '-Regular'
def sub(src, dst, cps, family=None):
    o = subset.Options(); o.flavor = 'woff2'; o.layout_features = ['*']; o.name_IDs = ['*']
    f = TTFont(src); s = subset.Subsetter(o); s.populate(unicodes=cps); s.subset(f)
    if family: rename(f, family)
    f.flavor = 'woff2'; f.save(dst)
    print(dst)
out = 'internal/web/static/fonts/'
sub(mono, out + 'labmono-regular.woff2', text | icons)
sub('/usr/share/fonts/TTF/JetBrainsMonoNerdFont-Bold.ttf', out + 'labmono-bold.woff2', text)
if len(sys.argv) < 2:
    sys.exit('usage: fonts.py <path to IBMPlexSans[wdth,wght].ttf>')
sub(sys.argv[1], out + 'labsans.woff2', text, 'Lab Sans')
